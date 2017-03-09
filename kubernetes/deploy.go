package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/golang/glog"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"

	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultNamespace string = "default"

type KubernetesDeployment struct {
	BastionIp       string
	MasterIp        string
	KubeConfigPath  string
	KubeConfig      *rest.Config
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

func (k8sDeployment *KubernetesDeployment) populateNodeInfos(ec2Svc *ec2.EC2) error {
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
			{
				Name: aws.String("instance-state-name"),
				Values: []*string{
					aws.String("running"),
				},
			},
		},
	}
	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	i := 1
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			nodeInfo := &awsecs.NodeInfo{
				Instance:  instance,
				PrivateIp: *instance.PrivateIpAddress,
			}
			glog.Infof("Adding node info i: %d, info: %v", i, nodeInfo)
			k8sDeployment.DeployedCluster.NodeInfos[i] = nodeInfo
			i += 1
		}
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) uploadFiles(ec2Svc *ec2.EC2, uploadedFiles map[string]string) error {
	if len(k8sDeployment.DeployedCluster.Deployment.Files) == 0 {
		return nil
	}

	deployedCluster := k8sDeployment.DeployedCluster

	clientConfig, clientConfigErr := deployedCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	for _, nodeInfo := range deployedCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PrivateIp+":22", clientConfig, k8sDeployment.BastionIp+":22")
		// TODO: Refactor this so can be reused with AWS
		for _, deployFile := range deployedCluster.Deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := sshClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				return fmt.Errorf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, nodeInfo.PrivateIp, err.Error())
			}
		}
	}

	glog.Info("Uploaded all files")
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
		TemplateURL:      aws.String("https://hyperpilot-k8s.s3.amazonaws.com/kubernetes-cluster-with-new-vpc.template"),
		TimeoutInMinutes: aws.Int64(30),
	}
	glog.Info("Creating kubernetes stack...")
	if _, err := cfSvc.CreateStack(params); err != nil {
		return errors.New("Unable to create stack: " + err.Error())
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(k8sDeployment.DeployedCluster.StackName()),
	}

	glog.Info("Waiting until stack is completed...")
	if err := cfSvc.WaitUntilStackCreateComplete(describeStacksInput); err != nil {
		return errors.New("Unable to wait until stack complete: " + err.Error())
	}

	glog.Info("Kuberenete stack completed")
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

	k8sDeployment.BastionIp = addresses[0]
	k8sDeployment.MasterIp = addresses[1]

	if err := k8sDeployment.downloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}

	glog.Infof("Downloaded kube config at %s", k8sDeployment.KubeConfigPath)

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployment.KubeConfigPath); err != nil {
		return errors.New("Unable to parse kube config: " + err.Error())
	} else {
		k8sDeployment.KubeConfig = kubeConfig
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

	if err := k8sDeployment.populateNodeInfos(ec2Svc); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := k8sDeployment.uploadFiles(ec2Svc, uploadedFiles); err != nil {
		//k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	if err := k8sDeployment.tagKubeNodes(); err != nil {
		//k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sDeployment.deployServices(); err != nil {
		//k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (k8sClusters *KubernetesClusters) UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Updating kubernetes deployment")

	k8sDeployment := &KubernetesDeployment{
		DeployedCluster: deployedCluster,
	}

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", "/tmp/delivery_kubeconfig/kubeconfig"); err != nil {
		return errors.New("Unable to parse kube config: " + err.Error())
	} else {
		k8sDeployment.KubeConfig = kubeConfig
	}

	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	if err := k8sDeployment.populateNodeInfos(ec2Svc); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := k8sDeployment.deployServices(); err != nil {
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (k8sClusters *KubernetesClusters) DeleteDeployment(config *viper.Viper, deployedCluster *awsecs.DeployedCluster) {
	k8sDeployment, ok := k8sClusters.Clusters[deployedCluster.Deployment.Name]
	if !ok {
		glog.Warningf("Unable to find kubernetes deployment to delete: %s", deployedCluster.Deployment.Name)
		return
	}

	if k8sDeployment.KubeConfig != nil {
		glog.Infof("Found kube config, deleting kubernetes objects")
		if c, err := k8s.NewForConfig(k8sDeployment.KubeConfig); err == nil {
			deploys := c.Extensions().Deployments(defaultNamespace)
			if deployLists, listError := deploys.List(v1.ListOptions{}); listError == nil {
				for _, deployment := range deployLists.Items {
					deploymentName := deployment.GetObjectMeta().GetName()
					if err := deploys.Delete(deploymentName, &v1.DeleteOptions{}); err != nil {
						glog.Warningf("Unable to delete deployment %s: %s", deploymentName, err.Error())
					}
				}
			} else {
				glog.Warning("Unable to list deployments for deletion: " + listError.Error())
			}

			replicaSets := c.Extensions().ReplicaSets(defaultNamespace)
			if replicaSetLists, listError := replicaSets.List(v1.ListOptions{}); listError == nil {
				for _, replicaSet := range replicaSetLists.Items {
					replicaName := replicaSet.GetObjectMeta().GetName()
					if err := replicaSets.Delete(replicaName, &v1.DeleteOptions{}); err != nil {
						glog.Warningf("Unable to delete replica set %s: %s", replicaName, err.Error())
					}
				}
			} else {
				glog.Warning("Unable to list replica sets for deletion: " + listError.Error())
			}

			pods := c.CoreV1().Pods(defaultNamespace)
			if podLists, listError := pods.List(v1.ListOptions{}); listError == nil {
				for _, pod := range podLists.Items {
					podName := pod.GetObjectMeta().GetName()
					if err := pods.Delete(podName, &v1.DeleteOptions{}); err != nil {
						glog.Warningf("Unable to delete pod %s: %s", podName, err.Error())
					}
				}
			} else {
				glog.Warning("Unable to list pods for deletion: " + listError.Error())
			}

			services := c.CoreV1().Services(defaultNamespace)
			if serviceLists, listError := services.List(v1.ListOptions{}); listError == nil {
				for _, service := range serviceLists.Items {
					serviceName := service.GetObjectMeta().GetName()
					if err := services.Delete(serviceName, &v1.DeleteOptions{}); err != nil {
						glog.Warningf("Unable to delete service %s: %s", serviceName, err.Error())
					}
				}
			} else {
				glog.Warning("Unable to list services for deletion: " + listError.Error())
			}
		} else {
			glog.Warning("Unable to connect to kubernetes, skipping delete kubernetes objects")
		}
	}

	/*
				sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
				if sessionErr != nil {
					glog.Warningf("Unable to create aws session for delete: %s", sessionErr.Error())
					return
				}

				cfSvc := cloudformation.New(sess)
				ec2Svc := ec2.New(sess)

				deleteStackInput := &cloudformation.DeleteStackInput{
					StackName: aws.String(deployedCluster.StackName()),
				}

				if _, err := cfSvc.DeleteStack(deleteStackInput); err != nil {
					glog.Warning("Unable to delete stack: " + err.Error())
				}

		                describeStacksInput = &cloudFormation.DescribeStacksInput{
		                        StackName: aws.String(deployedCluster.StackName()),
		                }

		                if err := cfSvc.WaitUntilStackDeleteComplete(describeStacksInput); err != nil {
		                        glog.Warning("Unable to wait until stack is deleted: " + err.Error())
		                }

				if err := awsecs.DeleteKeyPair(ec2Svc, deployedCluster); err != nil {
					glog.Warning("Unable to delete key pair: " + err.Error())
				}

				delete(k8sClusters.Clusters, deployedCluster.Deployment.Name)
	*/
}

func (k8sDeployment *KubernetesDeployment) tagKubeNodes() error {
	//TODO: Tag kube nodes with node mapping ids, and then remember the mapping
	// in a in-memory structure.
	// We should do this so we know consistently which tasks are running on which nodes.
	return nil
}

func (k8sDeployment *KubernetesDeployment) deployServices() error {
	if k8sDeployment.KubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	deployedCluster := k8sDeployment.DeployedCluster

	if c, err := k8s.NewForConfig(k8sDeployment.KubeConfig); err == nil {
		deploy := c.Extensions().Deployments(defaultNamespace)
		for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
			deploySpec := task.Deployment
			family := task.Family

			// Assigning Pods to Nodes
			nodeSelector := map[string]string{}
			node, nodeErr := getNode(family, deployedCluster)
			if nodeErr != nil {
				return errors.New("Unable to find node: " + err.Error())
			}

			//publicDnsName := *node.Instance.PublicDnsName
			//nodeName := strings.TrimSuffix(*node.Instance.PrivateDnsName, ".ec2.internal")
			nodeName := *node.Instance.PrivateDnsName
			glog.Infof("Selecting node %s for deployment %s", nodeName, family)
			nodeSelector["kubernetes.io/hostname"] = nodeName

			deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

			// Create service for each container that opens a port
			for _, container := range deploySpec.Spec.Template.Spec.Containers {
				if len(container.Ports) == 0 {
					continue
				}

				service := c.CoreV1().Services(defaultNamespace)
				serviceName := family
				if family != container.Name {
					serviceName = serviceName + "-" + container.Name
				}
				labels := map[string]string{"app": family}
				servicePorts := []v1.ServicePort{}
				for i, port := range container.Ports {
					newPort := v1.ServicePort{
						Port:       port.HostPort,
						TargetPort: intstr.FromInt(int(port.ContainerPort)),
						Name:       "port" + strconv.Itoa(i),
					}
					servicePorts = append(servicePorts, newPort)
				}

				v1Service := &v1.Service{
					ObjectMeta: v1.ObjectMeta{
						Name:      serviceName,
						Labels:    labels,
						Namespace: defaultNamespace,
					},
					Spec: v1.ServiceSpec{
						Type:     v1.ServiceTypeClusterIP,
						Ports:    servicePorts,
						Selector: labels,
					},
				}
				_, err = service.Create(v1Service)
				if err != nil {
					return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
				}
				glog.Infof("Created %s service", serviceName)
			}

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

	nodeInfo, ok := deployedCluster.NodeInfos[id]
	if !ok {
		return nil, errors.New("Unable to find node in node infos")
	}

	return nodeInfo, nil
}

// DownloadKubeConfig is Use SSHProxyCommand download k8s master node's kubeconfig
func (k8sDeployment *KubernetesDeployment) downloadKubeConfig() error {
	deployedCluster := k8sDeployment.DeployedCluster
	baseDir := deployedCluster.Deployment.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	clientConfig, clientConfigErr := k8sDeployment.DeployedCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	sshClient := common.NewSshClient(k8sDeployment.MasterIp+":22", clientConfig, k8sDeployment.BastionIp+":22")
	if err := sshClient.CopyRemoteFileToLocal("/home/ubuntu/kubeconfig", kubeconfigFilePath); err != nil {
		return errors.New("Unable to copy kubeconfig file to local: " + err.Error())
	}

	k8sDeployment.KubeConfigPath = kubeconfigFilePath
	return nil
}
