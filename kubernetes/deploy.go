package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"
	"golang.org/x/crypto/ssh"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"

	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

const defaultNamespace string = "default"

type KubernetesDeployment struct {
	BastionIp       string
	MasterIp        string
	KubeConfigPath  string
	DeployedCluster *awsecs.DeployedCluster
}

type KubernetesClusters struct {
	Clusters map[string]*KubernetesDeployment
}

func NewKubernetesClusters() *KubernetesClusters {
	return &KubernetesClusters{
		Clusters: make(map[string]*KubernetesDeployment),
	}
}

func waitUntilMasterReady(publicDNSName string, timeout time.Duration) error {
	// Use client-go to poll kube master until it's ready.
	config := &rest.Config{
		Host: publicDNSName + ":8080",
	}

	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				if _, err := k8s.NewForConfig(config); err == nil {
					c <- true
				}
				time.Sleep(time.Second * 2)
			}
		}
	}()

	select {
	case <-c:
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for master to be ready")
	}
}

func (k8sDeployment *KubernetesDeployment) uploadFiles(ec2Svc *ec2.EC2, uploadedFiles map[string]string) error {
	if len(k8sDeployment.DeployedCluster.Deployment.Files) == 0 {
		return nil
	}

	deployedCluster := k8sDeployment.DeployedCluster

	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("k8s-node"),
				},
			},
			{
				Name: aws.String("tag:KubernetesCluster"),
				Values: []*string{
					aws.String(k8sDeployment.DeployedCluster.StackName()),
				},
			},
		},
	}
	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	instanceIps := make([]*string, 0)
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			instanceIps = append(instanceIps, instance.PrivateIpAddress)
		}
	}

	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}

	for _, instanceIp := range instanceIps {
		sshClient := common.NewSshClient(*instanceIp, clientConfig, k8sDeployment.BastionIp)
		// TODO: Refactor this so can be reused with AWS
		for _, deployFile := range deployedCluster.Deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			f, fileErr := os.Open(location)
			if fileErr != nil {
				return errors.New(
					"Unable to open uploaded file " + deployFile.FileId +
						": " + fileErr.Error())
			}
			defer f.Close()

			if err := sshClient.CopyFile(f, deployFile.Path, "0644"); err != nil {
				return fmt.Errorf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, *instanceIp, err.Error())
			}
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deployKubernetesCluster(sess *session.Session) error {
	cfSvc := cloudformation.New(sess)
	params := &cloudformation.CreateStackInput{
		StackName: aws.String(k8sDeployment.DeployedCluster.StackName()),
		Capabilities: []*string{
			aws.String("CAPABILITY_NAMED_IAM"),
		},
		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("AdminIngressLocation"),
				ParameterValue: aws.String("0.0.0.0/0"),
			},
			{
				ParameterKey:   aws.String("KeyName"),
				ParameterValue: aws.String(k8sDeployment.DeployedCluster.KeyName()),
			},
			{
				ParameterKey:   aws.String("NetworkingProvider"),
				ParameterValue: aws.String("weave"),
			},
			{
				ParameterKey: aws.String("K8sNodeCapacity"),
				ParameterValue: aws.String(
					strconv.Itoa(
						len(k8sDeployment.DeployedCluster.Deployment.ClusterDefinition.Nodes))),
			},
			{
				ParameterKey: aws.String("InstanceType"),
				ParameterValue: aws.String(
					k8sDeployment.DeployedCluster.Deployment.ClusterDefinition.Nodes[0].InstanceType),
			},
			{
				ParameterKey:   aws.String("DiskSizeGb"),
				ParameterValue: aws.String("40"),
			},
			{
				ParameterKey:   aws.String("BastionInstanceType"),
				ParameterValue: aws.String("t2.micro"),
			},
			{
				ParameterKey:   aws.String("QSS3BucketName"),
				ParameterValue: aws.String("heptio-aws-quickstart-test"),
			},
			{
				ParameterKey:   aws.String("QSS3KeyPrefix"),
				ParameterValue: aws.String("heptio/kubernetes/latest"),
			},
			{
				ParameterKey:   aws.String("AvailabilityZone"),
				ParameterValue: aws.String(k8sDeployment.DeployedCluster.Deployment.Region + "b"),
			},
		},
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String("deployment"),
				Value: aws.String(k8sDeployment.DeployedCluster.Deployment.Name),
			},
		},
		TemplateURL:      aws.String("https://s3.amazonaws.com/quickstart-reference/heptio/latest/templates/kubernetes-cluster-with-new-vpc.template"),
		TimeoutInMinutes: aws.Int64(30),
	}
	if resp, err := cfSvc.CreateStack(params); err != nil {
		k8sDeployment.DeployedCluster.StackId = *resp.StackId
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(k8sDeployment.DeployedCluster.StackName()),
	}

	if err := cfSvc.WaitUntilStackCreateComplete(describeStacksInput); err != nil {
		return errors.New("Unable to wait until stack complete: " + err.Error())
	}

	describeStacksOutput, describeStacksErr := cfSvc.DescribeStacks(describeStacksInput)
	if describeStacksErr != nil {
		return errors.New("Unable to get stack outputs: " + describeStacksErr.Error())
	}

	outputs := describeStacksOutput.Stacks[0].Outputs
	sshProxyCommand := ""
	getKubeConfigCommand := ""
	for _, output := range outputs {
		if *output.OutputKey == "SSHProxyCommand" {
			sshProxyCommand = *output.OutputValue
		} else if *output.OutputKey == "GetKubeConfigCommand" {
			getKubeConfigCommand = *output.OutputValue
		}
	}

	if sshProxyCommand == "" {
		return errors.New("Unable to find SSHProxyCommand in stack output")
	} else if getKubeConfigCommand == "" {
		return errors.New("Unable to find GetKubeConfigCommand in stack output")
	}

	// We expect the SSHProxyCommand to be in the following format:
	//"ssh -A -L8080:localhost:8080 -o ProxyCommand='ssh ubuntu@111.111.111.111 nc 10.0.0.0 22' ubuntu@10.0.0.0"
	addresses := []string{}
	for _, part := range strings.Split(sshProxyCommand, " ") {
		if strings.HasPrefix(part, "ubuntu@") {
			addresses = append(addresses, strings.Split(part, "@")[1])
		}
	}

	if len(addresses) != 2 {
		return errors.New("Unexpected ssh proxy command format: " + sshProxyCommand)
	}

	k8sDeployment.BastionIp = addresses[1]
	k8sDeployment.MasterIp = addresses[0]

	if kubeConfigPath, err := k8sDeployment.downloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	} else if kubeConfigPath == nil {
		return errors.New("Kubeconfig downloaded path is empty")
	} else {
		k8sDeployment.KubeConfigPath = *kubeConfigPath
	}

	return nil
}

// CreateDeployment start a deployment
func (k8sClusters *KubernetesClusters) CreateDeployment(config *viper.Viper, uploadedFiles map[string]string, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")

	k8sDeployment := &KubernetesDeployment{
		DeployedCluster: deployedCluster,
	}

	k8sClusters.Clusters[deployedCluster.Deployment.Name] = k8sDeployment

	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	if err := awsecs.CreateKeypair(ec2Svc, deployedCluster); err != nil {
		return errors.New("Unable to create key pair: " + err.Error())
	}

	if err := k8sDeployment.deployKubernetesCluster(sess); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := k8sDeployment.uploadFiles(ec2Svc, uploadedFiles); err != nil {
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	if err := k8sDeployment.deployServices(); err != nil {
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (k8sClusters *KubernetesClusters) UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Updating kubernetes deployment")
	k8sDeployment, _ := k8sClusters.Clusters[deployment.Name]

	if err := k8sDeployment.deployServices(); err != nil {
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (k8sClusters *KubernetesClusters) DeleteDeployment(config *viper.Viper, deployedCluster *awsecs.DeployedCluster) {
	restConfig := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(restConfig); err == nil {
		deploys := c.Extensions().Deployments(defaultNamespace)
		if deployLists, listError := deploys.List(v1.ListOptions{}); listError == nil {
			for _, deployment := range deployLists.Items {
				deploys.Delete(deployment.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		replicaSets := c.Extensions().ReplicaSets(defaultNamespace)
		if replicaSetLists, listError := replicaSets.List(v1.ListOptions{}); listError == nil {
			for _, replicaSet := range replicaSetLists.Items {
				replicaSets.Delete(replicaSet.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		pods := c.CoreV1().Pods(defaultNamespace)
		if podLists, listError := pods.List(v1.ListOptions{}); listError == nil {
			for _, pod := range podLists.Items {
				pods.Delete(pod.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		services := c.CoreV1().Services(defaultNamespace)
		if serviceLists, listError := services.List(v1.ListOptions{}); listError == nil {
			for _, service := range serviceLists.Items {
				services.Delete(service.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}
	}

	awsecs.DeleteDeployment(config, deployedCluster)

	delete(k8sClusters.Clusters, deployedCluster.Deployment.Name)
}

func (k8sDeployment *KubernetesDeployment) deployServices() error {
	deployedCluster := k8sDeployment.DeployedCluster

	config := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(config); err == nil {
		deploy := c.Extensions().Deployments(defaultNamespace)
		for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
			deploySpec := task.Deployment
			family := task.Family

			// Assigning Pods to Nodes
			nodeSelector := map[string]string{}
			//svcExternalName := ""
			if node, err := getNode(family, deployedCluster); err != nil {
				return errors.New("Unable to find node: " + err.Error())
			} else if node != nil {
				//svcExternalName = node.PublicDnsName
				nodeSelector["kubernetes.io/hostname"] = node.PrivateIp
			}
			deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

			// Create service for each task
			service := c.CoreV1().Services(defaultNamespace)

			labels := map[string]string{"app": family}
			v1Service := &v1.Service{
				ObjectMeta: v1.ObjectMeta{
					Name:      family,
					Labels:    labels,
					Namespace: defaultNamespace,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeClusterIP,
					//ExternalName: svcExternalName,
				},
			}
			_, err = service.Create(v1Service)
			if err != nil {
				return fmt.Errorf("Unable to create service %s: %s", family, err)
			}
			glog.Infof("Created %s service", family)

			// start deploy deployment
			_, err = deploy.Create(&deploySpec)
			if err != nil {
				return fmt.Errorf("could not create deployment: %s", err)
			}
			glog.Infof("%s deployment created", family)
		}
	}

	return nil
}

func getNode(family string, deployedCluster *awsecs.DeployedCluster) (*awsecs.NodeInfo, error) {
	id := -1
	for _, node := range deployedCluster.Deployment.NodeMapping {
		if node.Task == family {
			id = node.Id
			break
		}
	}

	if id == -1 {
		return nil, errors.New("Unable to get node by family:" + family)
	}

	return deployedCluster.NodeInfos[id], nil
}

// DownloadKubeConfig is Use SSHProxyCommand download k8s master node's kubeconfig
func (k8sDeployment *KubernetesDeployment) downloadKubeConfig() (*string, error) {
	deployedCluster := k8sDeployment.DeployedCluster
	baseDir := deployedCluster.Deployment.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	pemFilePath := basePath + "/" + deployedCluster.KeyName() + ".pem"
	os.Remove(pemFilePath)

	kubeconfigFilePath := basePath + "/kubeconfig"
	os.Remove(kubeconfigFilePath)

	file, err := os.Create(pemFilePath)
	if err != nil {
		return nil, errors.New("Unable to create pem file:" + deployedCluster.KeyName() + ".pem")
	}
	defer file.Close()

	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
	file.WriteString(privateKey)

	chmodCmd := exec.Command("chmod", "400", pemFilePath)
	if err := chmodCmd.Start(); err != nil {
		return nil, errors.New("Unable to chmod pem: " + err.Error())
	}

	sshAddCmd := exec.Command("ssh-add", pemFilePath)
	if err := sshAddCmd.Start(); err != nil {
		return nil, errors.New("Unable to ssh add pem: " + err.Error())
	}

	proxyCommand := fmt.Sprintf("ProxyCommand=ssh ubuntu@%s nc %s 22", k8sDeployment.BastionIp, k8sDeployment.MasterIp)
	targetRemotePath := fmt.Sprintf("ubuntu@%s:~/kubeconfig", k8sDeployment.MasterIp)
	kubeConfigCmd := exec.Command("scp", "-o", proxyCommand, targetRemotePath, kubeconfigFilePath)
	if err := kubeConfigCmd.Start(); err != nil {
		return nil, errors.New("Unable to run get kube config command: " + err.Error())
	}

	return &kubeconfigFilePath, nil
}
