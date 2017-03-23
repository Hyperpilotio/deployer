package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"

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
	Endpoints       map[string]string
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
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	if err := k8sDeployment.tagKubeNodes(); err != nil {
		//k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sDeployment.deployServices(); err != nil {
		k8sClusters.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	if err := k8sDeployment.tagEndpoints(); err != nil {
		return errors.New("Unable to tag deployment service endpoint: " + err.Error())
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (k8sClusters *KubernetesClusters) UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Updating kubernetes deployment")

	k8sDeployment := &KubernetesDeployment{
		DeployedCluster: deployedCluster,
	}

	if kubeConfig, err := clientcmd.BuildConfigFromFlags("", "/tmp/"+deployment.Name+"_kubeconfig/kubeconfig"); err != nil {
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

	if err := k8sDeployment.tagEndpoints(); err != nil {
		return errors.New("Unable to tag deployment service endpoint: " + err.Error())
	}

	return nil
}

func deleteSecurityGroup(ec2Svc *ec2.EC2, vpcID *string) error {
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcID},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	for _, group := range resp.SecurityGroups {
		if aws.StringValue(group.GroupName) == "default" {
			continue
		}
		params := &ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			glog.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func waitUntilInternetGatewayDeleted(ec2Svc *ec2.EC2, deploymentName string, timeout time.Duration) error {
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				params := &ec2.DescribeTagsInput{
					Filters: []*ec2.Filter{
						{
							Name: aws.String("resource-type"),
							Values: []*string{
								aws.String("internet-gateway"),
							},
						},
						{
							Name: aws.String("tag:deployment"),
							Values: []*string{
								aws.String(deploymentName),
							},
						},
					},
				}

				resp, _ := ec2Svc.DescribeTags(params)
				if resp.Tags == nil {
					c <- true
				}
				time.Sleep(time.Second * 10)
			}
		}
	}()

	select {
	case <-c:
		glog.Info("InternetGateway is deleted")
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for InternetGateway to be deleted")
	}
}

func deleteCfStack(elbSvc *elb.ELB, ec2Svc *ec2.EC2, cfSvc *cloudformation.CloudFormation, stackName string, k8sDeployment *KubernetesDeployment) error {
	// find k8s-node stack name
	describeStacksOutput, _ := cfSvc.DescribeStacks(nil)
	k8sNodeStackName := ""
	vpcID := ""
	for _, stack := range describeStacksOutput.Stacks {
		if strings.HasPrefix(aws.StringValue(stack.StackName), stackName+"-K8sStack") {
			k8sNodeStackName = aws.StringValue(stack.StackName)
			for _, param := range stack.Parameters {
				if aws.StringValue(param.ParameterKey) == "VPCID" {
					vpcID = aws.StringValue(param.ParameterValue)
					break
				}
			}
		}
	}

	// delete k8s-master/k8s-node stack
	glog.Infof("Deleting k8s-master/k8s-node stack...")
	deleteStackInput := &cloudformation.DeleteStackInput{
		StackName: aws.String(k8sNodeStackName),
	}

	if _, err := cfSvc.DeleteStack(deleteStackInput); err != nil {
		glog.Warningf("Unable to delete stack: %s", err.Error())
	}

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(k8sNodeStackName),
	}

	if err := cfSvc.WaitUntilStackDeleteComplete(describeStacksInput); err != nil {
		glog.Warningf("Unable to wait until stack is deleted: %s", err.Error())
	} else if err == nil {
		glog.Infof("Delete %s stack ok...", k8sNodeStackName)
	}

	// delete loadBalancer
	glog.Infof("Deleting loadBalancer...")
	resp, _ := elbSvc.DescribeLoadBalancers(nil)
	for _, lbd := range resp.LoadBalancerDescriptions {
		deleteLoadBalancerInput := &elb.DeleteLoadBalancerInput{LoadBalancerName: lbd.LoadBalancerName}
		if _, err := elbSvc.DeleteLoadBalancer(deleteLoadBalancerInput); err != nil {
			glog.Warningf("Unable to deleting loadBalancer: %s", err.Error())
		}
	}

	// delete bastion-Host EC2 instance
	glog.Infof("Deleting bastion-Host EC2 instance...")
	if err := deleteBastionHost(ec2Svc); err != nil {
		glog.Warningf("Unable to delete bastion-Host EC2 instance: %s", err.Error())
	}

	// delete bastion-host stack
	glog.Infof("Deleting bastion-host stack...")
	deleteBastionHostStackInput := &cloudformation.DeleteStackInput{
		StackName: aws.String(stackName),
	}

	if _, err := cfSvc.DeleteStack(deleteBastionHostStackInput); err != nil {
		glog.Warningf("Unable to delete stack: %s", err.Error())
	}

	if err := waitUntilInternetGatewayDeleted(ec2Svc, strings.TrimSuffix(stackName, "-stack"), time.Duration(3)*time.Minute); err != nil {
		glog.Warningf("Unable to wait for internetGateway to be deleted: %s", err.Error())
	}

	// delete securityGroup
	retryTimes := 5
	glog.Infof("Deleteing securityGroup...")
	for i := 1; i <= retryTimes; i++ {
		if err := deleteSecurityGroup(ec2Svc, aws.String(vpcID)); err != nil {
			glog.Warningf("Unable to delete securityGroup: %s, retrying %d time", err.Error(), i)
		} else if err == nil {
			glog.Infof("Delete securityGroup ok...")
			break
		}
		time.Sleep(time.Duration(30) * time.Second)
	}

	describeBastionHostStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackName),
	}

	if err := cfSvc.WaitUntilStackDeleteComplete(describeBastionHostStacksInput); err != nil {
		glog.Warningf("Unable to wait until stack is deleted: %s", err.Error())
	} else if err == nil {
		glog.Infof("Delete %s stack ok...", stackName)
	}

	return nil
}

func deleteBastionHost(ec2Svc *ec2.EC2) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("bastion-host"),
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

	var instanceIds []*string
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			instanceIds = append(instanceIds, instance.InstanceId)
		}
	}

	params := &ec2.TerminateInstancesInput{
		InstanceIds: instanceIds,
	}

	if _, err := ec2Svc.TerminateInstances(params); err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s\n", err.Error())
	}

	terminatedInstanceParams := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceTerminated(terminatedInstanceParams); err != nil {
		return fmt.Errorf("Unable to wait until EC2 instance terminated: %s\n", err.Error())
	}
	return nil
}

func deleteK8S(kubeConfig *rest.Config) error {
	if kubeConfig != nil {
		glog.Info("Found kube config, deleting kubernetes objects")
		if c, err := k8s.NewForConfig(kubeConfig); err == nil {
			deploys := c.Extensions().Deployments(defaultNamespace)
			if deployLists, listError := deploys.List(v1.ListOptions{}); listError == nil {
				for _, deployment := range deployLists.Items {
					deploymentName := deployment.GetObjectMeta().GetName()
					if err := deploys.Delete(deploymentName, &v1.DeleteOptions{}); err != nil {
						return fmt.Errorf("Unable to delete deployment %s: %s", deploymentName, err.Error())
					}
				}
			} else {
				return fmt.Errorf("Unable to list deployments for deletion: " + listError.Error())
			}

			replicaSets := c.Extensions().ReplicaSets(defaultNamespace)
			if replicaSetLists, listError := replicaSets.List(v1.ListOptions{}); listError == nil {
				for _, replicaSet := range replicaSetLists.Items {
					replicaName := replicaSet.GetObjectMeta().GetName()
					if err := replicaSets.Delete(replicaName, &v1.DeleteOptions{}); err != nil {
						return fmt.Errorf("Unable to delete replica set %s: %s", replicaName, err.Error())
					}
				}
			} else {
				return fmt.Errorf("Unable to list replica sets for deletion: " + listError.Error())
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
				glog.Warningf("Unable to list services for deletion: " + listError.Error())
			}
		} else {
			glog.Warningf("Unable to connect to kubernetes, skipping delete kubernetes objects")
		}
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

	// Deleting kubernetes deployment
	glog.Infof("Deleting kubernetes deployment...")
	if err := deleteK8S(k8sDeployment.KubeConfig); err != nil {
		glog.Warningf("Unable to deleting kubernetes deployment: %s", err.Error())
	}

	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment)
	if sessionErr != nil {
		glog.Warningf("Unable to create aws session for delete: %s", sessionErr.Error())
		return
	}

	elbSvc := elb.New(sess)
	ec2Svc := ec2.New(sess)
	cfSvc := cloudformation.New(sess)

	// deregister loadBalancer
	glog.Infof("Deregister loadBalancer...")
	resp, _ := elbSvc.DescribeLoadBalancers(nil)
	for _, lbd := range resp.LoadBalancerDescriptions {
		deregisterInstancesFromLoadBalancerInput := &elb.DeregisterInstancesFromLoadBalancerInput{
			Instances:        lbd.Instances,
			LoadBalancerName: lbd.LoadBalancerName,
		}
		if _, err := elbSvc.DeregisterInstancesFromLoadBalancer(deregisterInstancesFromLoadBalancerInput); err != nil {
			glog.Warningf("Unable to deregister instances from loadBalancer: %s:", err.Error())
		}
	}

	// delete cloudformation Stack
	cfStackName := deployedCluster.StackName()
	glog.Infof("Deleting cloudformation Stack: %s", cfStackName)
	if err := deleteCfStack(elbSvc, ec2Svc, cfSvc, cfStackName, k8sDeployment); err != nil {
		glog.Warningf("Unable to deleting cloudformation Stack: %s", err.Error())
	}

	glog.Infof("Deleting KeyPair...")
	if err := awsecs.DeleteKeyPair(ec2Svc, deployedCluster); err != nil {
		glog.Warning("Unable to delete key pair: " + err.Error())
	}

	delete(k8sClusters.Clusters, deployedCluster.Deployment.Name)
}

func (k8sDeployment *KubernetesDeployment) tagKubeNodes() error {
	nodeInfos := map[string]int{}
	for _, mapping := range k8sDeployment.DeployedCluster.Deployment.NodeMapping {
		privateDnsName := k8sDeployment.DeployedCluster.NodeInfos[mapping.Id].Instance.PrivateDnsName
		nodeInfos[aws.StringValue(privateDnsName)] = mapping.Id
	}

	if c, err := k8s.NewForConfig(k8sDeployment.KubeConfig); err == nil {
		for nodeName, id := range nodeInfos {
			if node, err := c.CoreV1().Nodes().Get(nodeName); err == nil {
				node.Labels["hyperpilot/node-id"] = strconv.Itoa(id)
				if _, err := c.CoreV1().Nodes().Update(node); err == nil {
					glog.V(1).Infof("Added label hyperpilot/node-id:%s to Kubernetes node %s", strconv.Itoa(id), nodeName)
				}
			} else {
				return fmt.Errorf("Unable to get Kubernetes node by name %s: %s", nodeName, err.Error())
			}
		}
	} else {
		return fmt.Errorf("Unable to connect to Kubernetes master for tagging nodes: %s", err.Error())
	}

	return nil
}

func (k8sDeployment *KubernetesDeployment) deployServices() error {
	if k8sDeployment.KubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	deployedCluster := k8sDeployment.DeployedCluster

	tasks := map[string]apis.KubernetesTask{}
	for _, task := range k8sDeployment.DeployedCluster.Deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	if c, err := k8s.NewForConfig(k8sDeployment.KubeConfig); err == nil {
		deploy := c.Extensions().Deployments(defaultNamespace)
		taskCount := map[string]int{}
		for _, mapping := range k8sDeployment.DeployedCluster.Deployment.NodeMapping {
			glog.Infof("Deploying task %s with mapping %d", mapping.Task, mapping.Id)

			task, ok := tasks[mapping.Task]
			if !ok {
				return fmt.Errorf("Unable to find task %s in task definitions", mapping.Task)
			}

			deploySpec := task.Deployment
			family := task.Family
			node, ok := deployedCluster.NodeInfos[mapping.Id]
			if !ok {
				return fmt.Errorf("Unable to find node id %s in cluster", mapping.Id)
			}

			count, ok := taskCount[family]
			if !ok {
				count = 1
			} else {
				count += 1
				family = family + "-" + strconv.Itoa(count)
				newName := deploySpec.GetObjectMeta().GetName() + "-" + strconv.Itoa(count)
				newLabels := map[string]string{"app": newName}

				// Update deploy spec to reflect multiple count of the same task
				deploySpec.GetObjectMeta().SetName(newName)
				deploySpec.GetObjectMeta().SetLabels(newLabels)
				deploySpec.Spec.Selector.MatchLabels = newLabels
				deploySpec.Spec.Template.GetObjectMeta().SetLabels(newLabels)
			}
			taskCount[family] = count

			// Assigning Pods to Nodes
			nodeSelector := map[string]string{}
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
				if !strings.HasPrefix(family, serviceName) {
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

				internalService := &v1.Service{
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
				_, err = service.Create(internalService)
				if err != nil {
					return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
				}
				glog.Infof("Created %s internal service", serviceName)

				// Check the type of each port opened by the container; create a loadbalancer service to expose the public port
				if task.PortTypes != nil && len(task.PortTypes) > 0 {
					for i, portType := range task.PortTypes {
						if portType == 1 { // public port
							publicServiceName := serviceName + "-public" + servicePorts[i].Name
							publicService := &v1.Service{
								ObjectMeta: v1.ObjectMeta{
									Name:      publicServiceName,
									Labels:    labels,
									Namespace: defaultNamespace,
								},
								Spec: v1.ServiceSpec{
									Type: v1.ServiceTypeLoadBalancer,
									Ports: []v1.ServicePort{
										v1.ServicePort{
											Port:       servicePorts[i].Port,
											TargetPort: servicePorts[i].TargetPort,
											Name:       "public-" + servicePorts[i].Name,
										},
									},
									Selector: labels,
								},
							}
							_, err = service.Create(publicService)
							if err != nil {
								return fmt.Errorf("Unable to create public service %s: %s", publicServiceName, err)
							}
							glog.Infof("Created a public service %s with port %d", publicServiceName, servicePorts[i].Port)
						} else { // private port
							glog.Infof("Skipping creating public endpoint for service %s as it's marked as private", serviceName)
						}
					}
				}
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

func (k8sDeployment *KubernetesDeployment) waitUntilElbLoadBalancerReady(clientset *k8s.Clientset, timeout time.Duration) error {
	services := clientset.CoreV1().Services(defaultNamespace)
	endpoints := map[string]string{}
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				serviceReady := false
				if serviceLists, listError := services.List(v1.ListOptions{}); listError == nil {
					for _, service := range serviceLists.Items {
						serviceName := service.GetObjectMeta().GetName()
						if strings.Index(serviceName, "-public") != -1 {
							if len(service.Status.LoadBalancer.Ingress) > 0 {
								hostname := service.Status.LoadBalancer.Ingress[0].Hostname
								port := service.Spec.Ports[0].Port
								endpoints[serviceName] = hostname + ":" + strconv.FormatInt(int64(port), 10)

								serviceReady = true
							} else {
								serviceReady = false
								break
							}
						}
					}
				}

				if serviceReady {
					c <- true
				}
				time.Sleep(time.Second * 2)
			}
		}
	}()

	select {
	case <-c:
		glog.Info("ELB loadBalancer is ready")
		k8sDeployment.Endpoints = endpoints
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for Elb LoadBalancer to be ready")
	}
}

func (k8sDeployment *KubernetesDeployment) tagEndpoints() error {
	if k8sDeployment.KubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	if c, err := k8s.NewForConfig(k8sDeployment.KubeConfig); err == nil {
		// Wait until elb loadbanlcer is ready before kubectl describe services
		if err := k8sDeployment.waitUntilElbLoadBalancerReady(c, time.Duration(10)*time.Second); err != nil {
			return errors.New("Unable to wait for Elb LoadBalancer: " + err.Error())
		}
	}

	return nil
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
