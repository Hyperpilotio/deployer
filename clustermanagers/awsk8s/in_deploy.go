package awsk8s

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	k8sUtil "github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/go-utils/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

type InClusterK8SDeployer struct {
	K8SDeployer

	AutoScalingGroup *autoscaling.Group
	StackName        string
}

type ClusterNodes []v1.Node

func (c ClusterNodes) Len() int { return len(c) }
func (c ClusterNodes) Less(i, j int) bool {
	return c[i].CreationTimestamp.Before(c[j].CreationTimestamp)
}
func (c ClusterNodes) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

func NewInClusterDeployer(
	config *viper.Viper,
	deployment *apis.Deployment) (*InClusterK8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	awsCluster := hpaws.NewAWSCluster(deployment.Name, deployment.Region)
	awsCluster.AWSProfile = &hpaws.AWSProfile{
		AwsId:     config.GetString("awsId"),
		AwsSecret: config.GetString("awsSecret"),
	}

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("Unable to get in cluster kubeconfig: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return nil, errors.New("Unable to create in cluster k8s client: " + err.Error())
	}

	nodes, err := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("Unable to list kubernetes nodes: " + err.Error())
	}

	k8sNodes := ClusterNodes{}
	for _, node := range nodes.Items {
		k8sNodes = append(k8sNodes, node)
	}
	sort.Sort(k8sNodes)

	stackName := ""
	for _, node := range k8sNodes {
		if deployment, ok := node.Labels["hyperpilot/deployment"]; ok {
			stackName = deployment + "-stack"
			break
		}
	}

	if stackName == "" {
		return nil, errors.New("Unable to find deployment name in node labels")
	}

	deployer := &InClusterK8SDeployer{
		K8SDeployer: K8SDeployer{
			Config:        config,
			AWSCluster:    awsCluster,
			Deployment:    deployment,
			DeploymentLog: log,
			Services:      make(map[string]ServiceMapping),
			KubeConfig:    kubeConfig,
		},
		StackName: stackName,
	}

	return deployer, nil
}

func (deployer *InClusterK8SDeployer) findAutoscalingGroup(autoscalingSvc *autoscaling.AutoScaling) error {
	result, err := autoscalingSvc.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{})
	if err != nil {
		return fmt.Errorf("Unable to describe auto scaling groups: %s" + err.Error())
	}

	var autoScalingGroup *autoscaling.Group
	for _, group := range result.AutoScalingGroups {
		for _, tag := range group.Tags {
			if *tag.Key == "KubernetesCluster" && *tag.Value == deployer.StackName {
				autoScalingGroup = group
				break
			}
		}
	}

	if autoScalingGroup == nil {
		return errors.New("Unable to find auto scaling group for stack: " + deployer.StackName)
	}

	deployer.AutoScalingGroup = autoScalingGroup
	return nil
}

func (deployer *InClusterK8SDeployer) getLaunchConfiguration(autoscalingSvc *autoscaling.AutoScaling) (*autoscaling.LaunchConfiguration, error) {
	groupName := deployer.AutoScalingGroup.AutoScalingGroupName
	output, err := autoscalingSvc.DescribeLaunchConfigurations(&autoscaling.DescribeLaunchConfigurationsInput{
		LaunchConfigurationNames: []*string{deployer.AutoScalingGroup.LaunchConfigurationName},
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to describe launch configurations for auto scaling group %s: %s", groupName)
	}

	if len(output.LaunchConfigurations) == 0 {
		return nil, errors.New("No launch configurations found for auto scaling group: " + *groupName)
	}

	return output.LaunchConfigurations[0], nil
}

func (deployer *InClusterK8SDeployer) setupEC2(
	ec2Svc *ec2.EC2,
	autoscalingSvc *autoscaling.AutoScaling) error {
	awsCluster := deployer.AWSCluster
	log := deployer.GetLog().Logger
	if err := deployer.findAutoscalingGroup(autoscalingSvc); err != nil {
		return errors.New("Unable to find autoscaling group: " + err.Error())
	}

	launchConfig, err := deployer.getLaunchConfiguration(autoscalingSvc)
	if err != nil {
		return errors.New("Unable to get launch configuration: " + err.Error())
	}

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
					aws.String(deployer.StackName),
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

	if len(describeInstancesOutput.Reservations) == 0 || len(describeInstancesOutput.Reservations[0].Instances) == 0 {
		return errors.New("Unable to find any running k8s node in cluster")
	}

	reservation := describeInstancesOutput.Reservations[0]
	instance := reservation.Instances[0]
	networkSpecs := []*ec2.InstanceNetworkInterfaceSpecification{}
	for i, networkInterface := range instance.NetworkInterfaces {
		groupIds := []*string{}
		for _, group := range networkInterface.Groups {
			groupIds = append(groupIds, group.GroupId)
		}

		networkSpec := &ec2.InstanceNetworkInterfaceSpecification{
			DeviceIndex:              aws.Int64(int64(i)),
			DeleteOnTermination:      aws.Bool(false),
			AssociatePublicIpAddress: aws.Bool(false),
			Groups:   groupIds,
			SubnetId: networkInterface.SubnetId,
		}
		networkSpecs = append(networkSpecs, networkSpec)
	}

	blockDeviceMappings := []*ec2.BlockDeviceMapping{}
	for _, mapping := range launchConfig.BlockDeviceMappings {
		blockDeviceMappings = append(blockDeviceMappings, &ec2.BlockDeviceMapping{
			DeviceName: mapping.DeviceName,
			Ebs: &ec2.EbsBlockDevice{
				VolumeSize: mapping.Ebs.VolumeSize,
				VolumeType: mapping.Ebs.VolumeType,
			},
		})
	}

	nodeCount := len(deployer.Deployment.ClusterDefinition.Nodes)
	for _, node := range deployer.Deployment.ClusterDefinition.Nodes {
		runInstancesInput := &ec2.RunInstancesInput{
			KeyName:             launchConfig.KeyName,
			ImageId:             launchConfig.ImageId,
			BlockDeviceMappings: blockDeviceMappings,
			EbsOptimized:        launchConfig.EbsOptimized,
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Name: launchConfig.IamInstanceProfile,
			},
			UserData:          launchConfig.UserData,
			NetworkInterfaces: networkSpecs,
			InstanceType:      aws.String(node.InstanceType),
			MinCount:          aws.Int64(1),
			MaxCount:          aws.Int64(1),
		}

		log.Infof("Sending run ec2 instances request: %+v", runInstancesInput)

		runResult, runErr := ec2Svc.RunInstances(runInstancesInput)
		if runErr != nil {
			return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
		}

		if len(runResult.Instances) != 1 {
			return fmt.Errorf("Unexpected instance results: %+v", runResult.Instances)
		}

		awsCluster.NodeInfos[node.Id] = &hpaws.NodeInfo{
			Instance:  runResult.Instances[0],
			PrivateIp: aws.StringValue(runResult.Instances[0].PrivateIpAddress),
		}
		awsCluster.InstanceIds = append(awsCluster.InstanceIds, runResult.Instances[0].InstanceId)
	}

	if len(awsCluster.InstanceIds) != nodeCount {
		return fmt.Errorf("Unable to find equal amount of nodes after ec2 create, expecting: %d, found: %d",
			nodeCount, len(awsCluster.InstanceIds))
	}

	describeInstancesInput = &ec2.DescribeInstancesInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceExists(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	if err := deployer.tagEC2Instance(ec2Svc); err != nil {
		return errors.New("Unable to tag ec2 instance: " + err.Error())
	}

	describeInstanceStatusInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	log.Infof("Waitng for %d EC2 instances to be status ok", nodeCount)
	if err := ec2Svc.WaitUntilInstanceStatusOk(describeInstanceStatusInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) tagEC2Instance(ec2Svc *ec2.EC2) error {
	awsCluster := deployer.AWSCluster
	log := deployer.GetLog().Logger
	errBool := false
	for nodeId, nodeInfo := range awsCluster.NodeInfos {
		tags := []*ec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String("k8s-node"),
			},
			{
				Key:   aws.String("KubernetesCluster"),
				Value: aws.String(deployer.StackName),
			},
			{
				Key:   aws.String("deployment"),
				Value: aws.String(deployer.Deployment.Name),
			},
			{
				Key:   aws.String("InternalCluster"),
				Value: aws.String(awsCluster.StackName()),
			},
			{
				Key:   aws.String("NodeId"),
				Value: aws.String(strconv.Itoa(nodeId)),
			},
		}

		tagParams := &ec2.CreateTagsInput{
			Resources: []*string{nodeInfo.Instance.InstanceId},
			Tags:      tags,
		}

		if _, err := ec2Svc.CreateTags(tagParams); err != nil {
			log.Warningf("Unable to create tags for new instances: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to create tags all the relative new instances")
	}

	return nil
}

// CreateDeployment start a deployment
func (deployer *InClusterK8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	if err := deployInCluster(deployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	return nil, nil
}

func deployInCluster(deployer *InClusterK8SDeployer, uploadedFiles map[string]string) error {
	awsCluster := deployer.AWSCluster
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	autoscalingSvc := autoscaling.New(sess)
	ec2Svc := ec2.New(sess)

	log.Infof("Launching EC2 instances")
	if err := deployer.setupEC2(ec2Svc, autoscalingSvc); err != nil {
		deployer.DeleteDeployment()
		return errors.New("Unable to setup EC2: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	nodeNames := []string{}
	for _, nodeInfo := range awsCluster.NodeInfos {
		nodeNames = append(nodeNames, aws.StringValue(nodeInfo.Instance.PrivateDnsName))
	}
	if err := k8sUtil.WaitUntilKubernetesNodeExists(k8sClient, nodeNames, time.Duration(2)*time.Minute, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, awsCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}
	recordPrivateEndpoints(deployer, k8sClient)

	return nil
}

func recordPrivateEndpoints(deployer *InClusterK8SDeployer, k8sClient *k8s.Clientset) {
	log := deployer.DeploymentLog.Logger
	namespaces := []string{deployer.getNamespace()}
	deployer.Services = map[string]k8sUtil.ServiceMapping{}

	for _, namespace := range namespaces {
		services, serviceError := k8sClient.CoreV1().Services(namespace).List(metav1.ListOptions{})
		if serviceError != nil {
			log.Warningf("Unable to list services for namespace '%s': %s", namespace, serviceError.Error())
			return
		}
		for _, service := range services.Items {
			serviceName := service.GetObjectMeta().GetName()
			port := service.Spec.Ports[0].Port
			serviceMapping := k8sUtil.ServiceMapping{
				PrivateUrl: serviceName + "." + namespace + ":" + strconv.FormatInt(int64(port), 10),
			}
			deployer.Services[serviceName] = serviceMapping
		}
	}
}

func (deployer *InClusterK8SDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset) error {
	log := deployer.DeploymentLog.Logger
	namespace := deployer.getNamespace()
	if err := k8sUtil.CreateSecretsByNamespace(k8sClient, namespace, deployer.Deployment); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	log.Infof("Granting node-reader permission to namespace %s", namespace)
	if err := k8sUtil.GrantNodeReaderPermissionToNamespace(k8sClient, namespace, log); err != nil {
		// Assumption: if the action of grant failed, it wouldn't affect the whole deployment process which
		// is why we don't return an error here.
		log.Warningf("Unable to grant node-reader permission to namespace %s: %s", namespace, err.Error())
	}

	existingNamespaces, namespacesErr := k8sUtil.GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := k8sUtil.DeployServices(deployer.Config, k8sClient, deployer.Deployment,
		namespace, existingNamespaces, "ubuntu", log); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) getNamespace() string {
	return strings.ToLower(deployer.AWSCluster.Name)
}

func populateInClusterNodeInfos(ec2Svc *ec2.EC2, awsCluster *hpaws.AWSCluster) error {
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String("k8s-node"),
				},
			},
			{
				Name: aws.String("tag:InternalCluster"),
				Values: []*string{
					aws.String(awsCluster.StackName()),
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

	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			nodeInfo := &hpaws.NodeInfo{
				Instance:  instance,
				PrivateIp: *instance.PrivateIpAddress,
			}

			for _, tag := range instance.Tags {
				if aws.StringValue(tag.Key) == "NodeId" {
					nodeId, err := strconv.Atoi(aws.StringValue(tag.Value))
					if err != nil {
						return errors.New("Unable to convert node id to int: " + err.Error())
					}
					awsCluster.NodeInfos[nodeId] = nodeInfo
					break
				}
			}
		}
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (deployer *InClusterK8SDeployer) UpdateDeployment(deployment *apis.Deployment) error {
	deployer.Deployment = deployment
	log := deployer.GetLog().Logger

	log.Info("Updating in-cluster kubernetes deployment")
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	namespace := deployer.getNamespace()
	if err := k8sUtil.DeleteK8S([]string{namespace}, deployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	k8sUtil.DeleteNodeReaderClusterRoleBindingToNamespace(k8sClient, namespace, log)

	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes: " + err.Error())
	}

	originalDeployment := deployer.Deployment
	deployer.Deployment = extensions
	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		deployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}

	deployer.Deployment = newDeployment
	return nil
}

func deleteInClusterDeploymentOnFailure(deployer *InClusterK8SDeployer) {
	log := deployer.GetLog().Logger
	if deployer.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	deployer.DeleteDeployment()
}

// DeleteDeployment clean up the cluster from kubenetes.
func (deployer *InClusterK8SDeployer) DeleteDeployment() error {
	if len(deployer.AWSCluster.InstanceIds) == 0 {
		return nil
	}

	log := deployer.GetLog().Logger
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to create k8s client: " + err.Error())
	}

	namespace := deployer.getNamespace()
	k8sUtil.DeleteNodeReaderClusterRoleBindingToNamespace(k8sClient, namespace, log)

	namespaces := k8sClient.CoreV1().Namespaces()
	if err := namespaces.Delete(namespace, &metav1.DeleteOptions{}); err != nil {
		log.Warningf("Unable to delete kubernetes namespace %s: %s", namespace, err.Error())
	}

	sess, sessionErr := hpaws.CreateSession(deployer.AWSCluster.AWSProfile, deployer.AWSCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)

	// We have to wait until the ec2 instances exists, otherwise we cannot terminate them.
	err = ec2Svc.WaitUntilInstanceExists(&ec2.DescribeInstancesInput{
		InstanceIds: deployer.AWSCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to wait for ec2 instances to exist in delete: " + err.Error())
	}

	err = ec2Svc.WaitUntilInstanceStatusOk(&ec2.DescribeInstanceStatusInput{
		InstanceIds: deployer.AWSCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
	}

	_, err = ec2Svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: deployer.AWSCluster.InstanceIds,
	})
	if err != nil {
		log.Warningf("Unable to terminate EC2 instance: %s", err.Error())
	}

	for _, nodeInfo := range deployer.AWSCluster.NodeInfos {
		nodeName := aws.StringValue(nodeInfo.Instance.PrivateDnsName)
		if k8sClient.CoreV1().Nodes().Delete(nodeName, &metav1.DeleteOptions{}); err != nil {
			log.Warningf("Unable to delete kubelet %s from api: %s", nodeName, err.Error())
		}
	}

	err = ec2Svc.WaitUntilInstanceTerminated(&ec2.DescribeInstancesInput{
		InstanceIds: deployer.AWSCluster.InstanceIds,
	})
	if err != nil {
		log.Warningf("Unable to wait for ec2 instances to terminated in delete: %s", err.Error())
	}

	filters := []*ec2.Filter{&ec2.Filter{
		Name:   aws.String("status"),
		Values: []*string{aws.String("available")},
	}}
	if err := deleteNetworkInterfaces(ec2Svc, filters, log); err != nil {
		log.Warningf("Unable to delete network interfaces: %s", err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	sess, sessionErr := hpaws.CreateSession(deployer.AWSCluster.AWSProfile, deployer.AWSCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	if err := populateInClusterNodeInfos(ec2Svc, deployer.AWSCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if len(deployer.AWSCluster.NodeInfos) == 0 {
		return errors.New("Unable to find in-cluster ec2 instance...")
	}

	awsCluster := deployer.AWSCluster
	for _, node := range awsCluster.NodeInfos {
		awsCluster.InstanceIds = append(awsCluster.InstanceIds, node.Instance.InstanceId)
	}

	autoscalingSvc := autoscaling.New(sess)
	if err := deployer.findAutoscalingGroup(autoscalingSvc); err != nil {
		return errors.New("Unable to find autoscaling group: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}

	recordPrivateEndpoints(deployer, k8sClient)

	return nil
}

func (deployer *InClusterK8SDeployer) GetStoreInfo() interface{} {
	return nil
}

func (deployer *InClusterK8SDeployer) NewStoreInfo() interface{} {
	return nil
}

func (deployer *InClusterK8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	if info, ok := deployer.Services[serviceName]; ok {
		deployer.GetLog().Logger.Infof("Found cached service url for service %s: %s", serviceName, info.PrivateUrl)
		return info.PrivateUrl, nil
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return "", errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	namespace := deployer.getNamespace()
	services, err := k8sClient.CoreV1().Services(namespace).List(metav1.ListOptions{})
	if err != nil {
		return "", errors.New("Unable to list services in the cluster: " + err.Error())
	}

	for _, service := range services.Items {
		if service.ObjectMeta.Name == serviceName {
			nodeId, _ := k8sUtil.FindNodeIdFromServiceName(deployer.Deployment, serviceName)
			port := service.Spec.Ports[0].Port
			serviceUrl := serviceName + "." + namespace + ":" + strconv.FormatInt(int64(port), 10)
			deployer.Services[serviceName] = k8sUtil.ServiceMapping{
				PrivateUrl: serviceUrl,
				NodeId:     nodeId,
			}
			deployer.GetLog().Logger.Infof("Found service url from k8s for service %s: %s", serviceUrl)
			return serviceUrl, nil
		}
	}

	return "", fmt.Errorf("Service [%s] not found in endpoints", serviceName)
}

// GetServiceAddress return ServiceAddress object
func (deployer *InClusterK8SDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	privateUrl, err := deployer.GetServiceUrl(serviceName)
	if err != nil {
		return nil, fmt.Errorf("Unable to get %s service address: ", serviceName, err.Error())
	}

	serviceUrls := strings.Split(privateUrl, ":")
	if len(serviceUrls) != 2 {
		return nil, fmt.Errorf("Unexpected array number...")
	}

	port, err := strconv.Atoi(serviceUrls[1])
	if err != nil {
		return nil, fmt.Errorf("Unable to convert %s port: ", serviceName, err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	namespace := deployer.getNamespace()
	services, err := k8sClient.CoreV1().Services(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("Unable to list services in the cluster: " + err.Error())
	}

	clusterIP := ""
	for _, service := range services.Items {
		if service.ObjectMeta.Name == serviceName {
			clusterIP = service.Spec.ClusterIP
			break
		}
	}

	if clusterIP == "" {
		return nil, fmt.Errorf("Unable to find service %s in services", serviceName)
	}

	return &apis.ServiceAddress{
		Host: clusterIP,
		Port: int32(port),
	}, nil
}

func (deployer *InClusterK8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	if len(deployer.AWSCluster.NodeInfos) < 0 {
		k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
		if err != nil {
			return nil, errors.New("Unable to connect to kubernetes: " + err.Error())
		}
		recordPrivateEndpoints(deployer, k8sClient)
	}

	nodeNameInfos := map[string]string{}
	for id, nodeInfo := range deployer.AWSCluster.NodeInfos {
		nodeNameInfos[strconv.Itoa(id)] = aws.StringValue(nodeInfo.Instance.PrivateDnsName)
	}

	serviceMappings := make(map[string]interface{})
	for serviceName, serviceMapping := range deployer.Services {
		if serviceMapping.NodeId == 0 {
			serviceNodeId, err := k8sUtil.FindNodeIdFromServiceName(deployer.Deployment, serviceName)
			if err != nil {
				return nil, fmt.Errorf("Unable to find %s node id: %s", serviceName, err.Error())
			}
			serviceMapping.NodeId = serviceNodeId
		}
		serviceMapping.NodeName = nodeNameInfos[strconv.Itoa(serviceMapping.NodeId)]
		serviceMappings[serviceName] = serviceMapping
	}

	return serviceMappings, nil
}
