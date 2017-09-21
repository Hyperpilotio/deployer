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
	"github.com/hyperpilotio/deployer/clusters"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/funcs"
	"github.com/hyperpilotio/go-utils/log"
	logging "github.com/op/go-logging"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
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

	awsProfile := &hpaws.AWSProfile{
		AwsId:     config.GetString("awsId"),
		AwsSecret: config.GetString("awsSecret"),
	}
	awsCluster := hpaws.NewAWSCluster(deployment.Name, deployment.Region, awsProfile)

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

func waitUntilKubernetesNodeExists(
	k8sClient *k8s.Clientset,
	awsCluster *hpaws.AWSCluster,
	timeout time.Duration,
	log *logging.Logger) error {
	nodeNames := []string{}
	for _, nodeInfo := range awsCluster.NodeInfos {
		nodeNames = append(nodeNames, aws.StringValue(nodeInfo.Instance.PrivateDnsName))
	}

	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		allExists := true
		for _, nodeName := range nodeNames {
			if _, err := k8sClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{}); err != nil {
				allExists = false
				break
			}
		}
		if allExists {
			log.Infof("%s Kubernetes nodes are available now", awsCluster.Name)
			return true, nil
		}
		return false, nil
	})
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
	awsCluster := deployer.AWSCluster
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return nil, fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	autoscalingSvc := autoscaling.New(sess)
	ec2Svc := ec2.New(sess)

	log.Infof("Launching EC2 instances")
	if err := deployer.setupEC2(ec2Svc, autoscalingSvc); err != nil {
		deployer.DeleteDeployment()
		return nil, errors.New("Unable to setup EC2: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	if err := waitUntilKubernetesNodeExists(k8sClient, awsCluster, time.Duration(2)*time.Minute, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, awsCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := deployer.deployKubernetesObjects(k8sClient, false); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	recordPrivateEndpoints(deployer, k8sClient)

	return nil, nil
}

func recordPrivateEndpoints(deployer *InClusterK8SDeployer, k8sClient *k8s.Clientset) {
	log := deployer.DeploymentLog.Logger
	namespaces := []string{deployer.getNamespace()}
	deployer.Services = map[string]ServiceMapping{}

	for _, namespace := range namespaces {
		services, serviceError := k8sClient.CoreV1().Services(namespace).List(metav1.ListOptions{})
		if serviceError != nil {
			log.Warningf("Unable to list services for namespace '%s': %s", namespace, serviceError.Error())
			return
		}
		for _, service := range services.Items {
			serviceName := service.GetObjectMeta().GetName()
			port := service.Spec.Ports[0].Port
			serviceMapping := ServiceMapping{
				PrivateUrl: serviceName + "." + namespace + ":" + strconv.FormatInt(int64(port), 10),
			}
			deployer.Services[serviceName] = serviceMapping
		}
	}
}

func (deployer *InClusterK8SDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset, skipDelete bool) error {
	log := deployer.DeploymentLog.Logger
	existingNamespaces, namespacesErr := k8sUtil.GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	namespace := deployer.getNamespace()
	if err := k8sUtil.CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
		return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
	}

	if err := deployer.createInClusterSecrets(k8sClient); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	log.Infof("Granting node-reader permission to namespace %s", namespace)
	if err := deployer.grantNodeReaderPermissionToNamespace(k8sClient, namespace); err != nil {
		// Assumption: if the action of grant failed, it wouldn't affect the whole deployment process which
		//is why we don't return an error here.
		log.Warningf("Unable to grant node-reader permission to namespace %s: %s", namespace, err.Error())
	}

	if err := deployer.deployServices(k8sClient); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) deployServices(k8sClient *k8s.Clientset) error {
	deployment := deployer.Deployment
	kubeConfig := deployer.KubeConfig
	log := deployer.GetLog().Logger

	if kubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	tasks := map[string]apis.KubernetesTask{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	taskCount := map[string]int{}

	// We sort before we create services because we want to have a deterministic way to assign
	// service ids
	sort.Sort(deployment.NodeMapping)
	namespace := deployer.getNamespace()
	deploy := k8sClient.Extensions().Deployments(namespace)
	for _, mapping := range deployment.NodeMapping {
		log.Infof("Deploying task %s with mapping %d", mapping.Task, mapping.Id)
		task, ok := tasks[mapping.Task]
		if !ok {
			return fmt.Errorf("Unable to find task %s in task definitions", mapping.Task)
		}

		deploySpec := task.Deployment
		if deploySpec == nil {
			return fmt.Errorf("Unable to find deployment in task %s", mapping.Task)
		}

		if deploySpec.Labels == nil {
			deploySpec.Labels = make(map[string]string)
		}
		if deploySpec.Spec.Template.Labels == nil {
			deploySpec.Spec.Template.Labels = make(map[string]string)
		}

		family := task.Family
		originalFamily := family
		count, ok := taskCount[family]
		if !ok {
			count = 1
			deploySpec.Name = originalFamily
			deploySpec.Labels["app"] = originalFamily
			deploySpec.Spec.Template.Labels["app"] = originalFamily
		} else {
			// Update deploy spec to reflect multiple count of the same task
			count += 1
			family = family + "-" + strconv.Itoa(count)
			deploySpec.Name = family
			deploySpec.Labels["app"] = family
			deploySpec.Spec.Template.Labels["app"] = family
		}

		if deploySpec.Spec.Selector != nil {
			deploySpec.Spec.Selector.MatchLabels = deploySpec.Spec.Template.Labels
		}

		taskCount[originalFamily] = count

		// Assigning Pods to Nodes
		nodeSelector := map[string]string{}
		log.Infof("Selecting node %d for deployment %s", mapping.Id, family)
		nodeSelector["hyperpilot/node-id"] = strconv.Itoa(mapping.Id)
		nodeSelector["hyperpilot/deployment"] = deployment.Name

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := deployer.createServiceForInClusterDeployment(k8sClient, family, task, container, log)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unable to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	restartCount := deployer.Config.GetInt("restartCount")
	err := funcs.LoopUntil(time.Minute*60, time.Second*20, func() (bool, error) {
		deployments, listErr := deploy.List(metav1.ListOptions{})
		if listErr != nil {
			return false, errors.New("Unable to list deployments: " + listErr.Error())
		}

		if len(deployments.Items) != len(deployment.NodeMapping) {
			return false, fmt.Errorf("Unexpected list of deployments: %d", len(deployments.Items))
		}

		pods, listErr := k8sClient.CoreV1().Pods(namespace).List(metav1.ListOptions{})
		if listErr != nil {
			return false, errors.New("Unable to list pods: " + listErr.Error())
		}
		for _, pod := range pods.Items {
			switch pod.Status.Phase {
			case "Pending":
				for _, condition := range pod.Status.Conditions {
					if condition.Reason == "Unschedulable" {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, condition.Message)
					}
				}
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.State.Waiting.Reason == "ImagePullBackOff" {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, containerStatus.State.Waiting.Message)
					}
				}
			case "Running":
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.RestartCount >= int32(restartCount) {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, containerStatus.State.Waiting.Message)
					}
				}
			}
		}

		for _, deployment := range deployments.Items {
			if deployment.Status.ReadyReplicas == 0 {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf("Unable to wait for deployments to be available: %s", err.Error())
	}

	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		if task.Deployment != nil {
			return fmt.Errorf("Cannot assign both daemonset and deployment to the same task: %s", task.Family)
		}

		daemonSet := task.DaemonSet
		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	return nil
}

func (deployer *InClusterK8SDeployer) getNamespace() string {
	return strings.ToLower(deployer.AWSCluster.Name)
}

func (deployer *InClusterK8SDeployer) createInClusterSecrets(k8sClient *k8s.Clientset) error {
	secrets := deployer.Deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	namespace := deployer.getNamespace()
	for _, secret := range secrets {
		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func deleteNodeReaderClusterRoleBindingToNamespace(k8sClient *k8s.Clientset, namespace string, log *logging.Logger) {
	roleName := "node-reader"
	roleBinding := roleName + "-binding-" + namespace

	log.Infof("Deleting cluster role binding %s for namespace %s", roleBinding, namespace)
	clusterRoleBinding := k8sClient.RbacV1beta1().ClusterRoleBindings()
	if err := clusterRoleBinding.Delete(roleBinding, &metav1.DeleteOptions{}); err != nil {
		log.Warningf("Unable to delete cluster role binding %s for namespace %s", roleName, roleBinding)
	}
	log.Infof("Successfully delete cluster role binding %s", roleBinding)
}

// grantNodeReaderPermissionToNamespace grant read permission to the default user belongs to specified namespace

func (deployer *InClusterK8SDeployer) grantNodeReaderPermissionToNamespace(k8sClient *k8s.Clientset, namespace string) error {
	log := deployer.DeploymentLog.Logger
	roleName := "node-reader"
	roleBinding := roleName + "-binding-" + namespace

	// check whether role-binding exists
	clusterRoleBindings := k8sClient.RbacV1beta1().ClusterRoleBindings()
	clusterRoleBindingList, err := clusterRoleBindings.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Unable to list role-binding in cluster scope: %s", err.Error())

	}
	for _, val := range clusterRoleBindingList.Items {
		if val.GetName() == roleBinding {
			log.Infof("Found role binding %s in cluster scope", roleBinding)
			return nil
		}
	}

	// check whether role exists or not
	clusterRoles := k8sClient.RbacV1beta1().ClusterRoles()
	roleList, err := clusterRoles.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf(`Unable to list roles in cluster scope: %s`, err.Error())
	}
	var roleExists bool
	for _, val := range roleList.Items {
		if val.GetName() == roleName {
			roleExists = true
		}
	}
	if !roleExists {
		return fmt.Errorf("Role %s doesn't exist in cluster scope", roleName)
	}

	log.Infof("Binding cluster role '%s' to namespace %s", roleName, namespace)
	roleBindingObject := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleBinding,
		},
		Subjects: []rbac.Subject{
			rbac.Subject{
				Kind:      rbac.ServiceAccountKind,
				Name:      "default",
				Namespace: namespace,
			},
		},
		RoleRef: rbac.RoleRef{
			APIGroup: "",
			Kind:     "ClusterRole",
			Name:     roleName,
		},
	}

	if _, err = clusterRoleBindings.Create(roleBindingObject); err != nil {
		return fmt.Errorf("Unable to create role binding for namespace %s in cluster scope: %s", namespace, err.Error())
	}
	log.Infof("Successfully binds cluster role '%s' to namespace '%s'", roleName, namespace)

	return nil
}

func (deployer *InClusterK8SDeployer) createServiceForInClusterDeployment(
	k8sClient *k8s.Clientset,
	family string,
	task apis.KubernetesTask,
	container v1.Container,
	log *logging.Logger) error {
	if len(container.Ports) == 0 {
		return nil
	}

	namespace := deployer.getNamespace()
	service := k8sClient.CoreV1().Services(namespace)
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeClusterIP,
			Ports:    servicePorts,
			Selector: labels,
		},
	}
	_, err := service.Create(internalService)
	if err != nil {
		return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
	}

	log.Infof("Created %s internal service", serviceName)
	return nil
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

	if err := k8sUtil.DeleteK8S([]string{deployer.getNamespace()}, deployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	deleteNodeReaderClusterRoleBindingToNamespace(k8sClient, deployer.getNamespace(), deployer.DeploymentLog.Logger)

	if err := deployer.deployKubernetesObjects(k8sClient, true); err != nil {
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
	if err := deployer.deployKubernetesObjects(k8sClient, true); err != nil {
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

	deleteNodeReaderClusterRoleBindingToNamespace(k8sClient, deployer.getNamespace(), deployer.DeploymentLog.Logger)

	namespaces := k8sClient.CoreV1().Namespaces()
	if err := namespaces.Delete(deployer.getNamespace(), &metav1.DeleteOptions{}); err != nil {
		log.Warningf("Unable to delete kubernetes namespace %s: %s", deployer.getNamespace(), err.Error())
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

func (deployer *InClusterK8SDeployer) GetCluster() clusters.Cluster {
	return deployer.AWSCluster
}

func (deployer *InClusterK8SDeployer) GetLog() *log.FileLog {
	return deployer.DeploymentLog
}

func (deployer *InClusterK8SDeployer) GetScheduler() *job.Scheduler {
	return nil
}

func (deployer *InClusterK8SDeployer) SetScheduler(sheduler *job.Scheduler) {
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
			nodeId, _ := findNodeIdFromServiceName(deployer.Deployment, serviceName)
			port := service.Spec.Ports[0].Port
			serviceUrl := serviceName + "." + namespace + ":" + strconv.FormatInt(int64(port), 10)
			deployer.Services[serviceName] = ServiceMapping{
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
			serviceNodeId, err := findNodeIdFromServiceName(deployer.Deployment, serviceName)
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
