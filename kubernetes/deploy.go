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

func deployKubernetesCluster(sess *session.Session, deployedCluster *awsecs.DeployedCluster) error {
	cfSvc := cloudformation.New(sess)
	params := &cloudformation.CreateStackInput{
		StackName: aws.String(deployedCluster.StackName()),
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
				ParameterValue: aws.String(deployedCluster.KeyName()),
			},
			{
				ParameterKey:   aws.String("NetworkingProvider"),
				ParameterValue: aws.String("weave"),
			},
			{
				ParameterKey:   aws.String("K8sNodeCapacity"),
				ParameterValue: aws.String(strconv.Itoa(len(deployedCluster.Deployment.ClusterDefinition.Nodes))),
			},
			{
				ParameterKey:   aws.String("InstanceType"),
				ParameterValue: aws.String(deployedCluster.Deployment.ClusterDefinition.Nodes[0].InstanceType),
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
				ParameterValue: aws.String(deployedCluster.Deployment.Region + "b"),
			},
		},
		Tags: []*cloudformation.Tag{
			{
				Key:   aws.String("deployment"),
				Value: aws.String(deployedCluster.Deployment.Name),
			},
		},
		TemplateURL:      aws.String("https://s3.amazonaws.com/quickstart-reference/heptio/latest/templates/kubernetes-cluster-with-new-vpc.template"),
		TimeoutInMinutes: aws.Int64(30),
	}
	if resp, err := cfSvc.CreateStack(params); err != nil {
		deployedCluster.StackId = *resp.StackId
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(deployedCluster.StackName()),
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

	glog.Infof("sshProxyCommand: %s", sshProxyCommand)
	glog.Infof("getKubeConfigCommand: %s", getKubeConfigCommand)
	if kubeconfigPath, err := DownloadKubeConfig(deployedCluster, getKubeConfigCommand); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	} else if kubeconfigPath != nil {
		// TODO clientcmd.BuildConfigFromFlags("", *kubeconfig)
		glog.Infof("kubeconfigPath: %s", kubeconfigPath)
	}

	return nil
}

// CreateDeployment start a deployment
func CreateDeployment(config *viper.Viper, uploadedFiles map[string]string, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")
	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	if err := awsecs.CreateKeypair(ec2Svc, deployedCluster); err != nil {
		return errors.New("Unable to create key pair: " + err.Error())
	}

	if err := deployKubernetesCluster(sess, deployedCluster); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := deployServices(deployedCluster); err != nil {
		DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")

	if err := deployServices(deployedCluster); err != nil {
		DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func DeleteDeployment(config *viper.Viper, deployedCluster *awsecs.DeployedCluster) error {
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

	return nil
}

func deployServices(deployedCluster *awsecs.DeployedCluster) error {
	config := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(config); err == nil {
		deploy := c.Extensions().Deployments(defaultNamespace)
		for _, task := range deployedCluster.Deployment.KubernetesDeployment.Kubernetes {
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
func DownloadKubeConfig(deployedCluster *awsecs.DeployedCluster, kubeConfigCommand string) (*string, error) {
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

	time.Sleep(time.Duration(3) * time.Second)

	chmodCmd := exec.Command("chmod", "400", pemFilePath)
	if err := chmodCmd.Start(); err != nil {
		fmt.Println(err)
	}

	sshAddCmd := exec.Command("ssh-add", pemFilePath)
	if err := sshAddCmd.Start(); err != nil {
		fmt.Println(err)
	}

	bastionHost := ""
	targetHost := ""
	result := strings.Split(kubeConfigCommand, " ")
	for i := range result {
		if strings.Contains(result[i], "@") {
			if strings.Contains(result[i], "kubeconfig") {
				targetCmd := strings.Split(result[i], ":")
				targetHost = strings.Split(targetCmd[0], "@")[1]
			} else {
				bastionHost = strings.Split(result[i], "@")[1]
			}
		}
	}

	proxyCommand := fmt.Sprintf("ProxyCommand=ssh ubuntu@%s nc %s 22", bastionHost, targetHost)
	targetRemotePath := fmt.Sprintf("ubuntu@%s:~/kubeconfig", targetHost)
	KubeConfigCmd := exec.Command("scp", "-o", proxyCommand, targetRemotePath, kubeconfigFilePath)
	if err := KubeConfigCmd.Start(); err != nil {
		fmt.Println(err)
	}

	return &kubeconfigFilePath, nil
}
