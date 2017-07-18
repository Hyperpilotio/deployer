package kubernetes

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

type InClusterK8SDeployer struct {
	K8SDeployer

	OriginalAWSCluster *hpaws.AWSCluster
	AutoScalingGroup   *autoscaling.Group
}

type OriginalClusterInfo struct {
	AWSCluster     *hpaws.AWSCluster
	BastionIp      string
	MasterIp       string
	KubeConfigPath string
	KubeConfig     *rest.Config
}

func NewInClusterDeployer(
	config *viper.Viper,
	awsProfile *hpaws.AWSProfile,
	deployment *apis.Deployment,
	originalClusterInfo *OriginalClusterInfo) (*InClusterK8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	originalAWSCluster := originalClusterInfo.AWSCluster
	awsCluster := hpaws.NewAWSCluster(deployment.Name, originalAWSCluster.Region, awsProfile)
	deployer := &InClusterK8SDeployer{
		K8SDeployer: K8SDeployer{
			Config:         config,
			AWSCluster:     awsCluster,
			Deployment:     deployment,
			DeploymentLog:  log,
			Services:       make(map[string]ServiceMapping),
			KubeConfigPath: originalClusterInfo.KubeConfigPath,
			BastionIp:      originalClusterInfo.BastionIp,
			MasterIp:       originalClusterInfo.MasterIp,
			KubeConfig:     originalClusterInfo.KubeConfig,
		},
		OriginalAWSCluster: originalClusterInfo.AWSCluster,
	}

	return deployer, nil
}

func (deployer *InClusterK8SDeployer) findAutoscalingGroup(autoscalingSvc *autoscaling.AutoScaling) error {
	result, err := autoscalingSvc.DescribeAutoScalingGroups(nil)
	if err != nil {
		return fmt.Errorf("Unable to describe auto scaling groups: %s" + err.Error())
	}

	var autoScalingGroup *autoscaling.Group
	for _, group := range result.AutoScalingGroups {
		for _, tag := range group.Tags {
			if *tag.Key == "KubernetesCluster" && *tag.Value == deployer.OriginalAWSCluster.StackName() {
				autoScalingGroup = group
				break
			}
		}
	}

	if autoScalingGroup == nil {
		return errors.New("Unable to find auto scaling group for stack: " + deployer.OriginalAWSCluster.StackName())
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

func setupEC2(deployer *InClusterK8SDeployer,
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
					aws.String(deployer.OriginalAWSCluster.StackName()),
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

	// We assume cluster definition has nodes for all the same type for now.
	node := deployer.Deployment.ClusterDefinition.Nodes[0]
	nodeCount := len(deployer.Deployment.ClusterDefinition.Nodes)
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
		MinCount:          aws.Int64(int64(nodeCount)),
		MaxCount:          aws.Int64(int64(nodeCount)),
	}
	glog.V(1).Infof("runInstancesInput: %+v", runInstancesInput)

	runResult, runErr := ec2Svc.RunInstances(runInstancesInput)
	if runErr != nil {
		return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
	}

	if len(runResult.Instances) != nodeCount {
		return fmt.Errorf("Unable to find equal amount of nodes after ec2 create, expecting: %d, found: %d",
			nodeCount, len(runResult.Instances))
	}

	for i, node := range deployer.Deployment.ClusterDefinition.Nodes {
		awsCluster.NodeInfos[node.Id] = &hpaws.NodeInfo{
			Instance: runResult.Instances[i],
		}
		awsCluster.InstanceIds = append(awsCluster.InstanceIds, runResult.Instances[i].InstanceId)
	}

	describeInstancesInput = &ec2.DescribeInstancesInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceExists(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String("Name"),
			Value: aws.String("k8s-node"),
		},
		{
			Key:   aws.String("KubernetesCluster"),
			Value: aws.String(deployer.OriginalAWSCluster.StackName()),
		},
		{
			Key:   aws.String("deployment"),
			Value: aws.String(deployer.OriginalAWSCluster.Name),
		},
		{
			Key:   aws.String("InternalCluster"),
			Value: aws.String(awsCluster.StackName()),
		},
	}

	tagParams := &ec2.CreateTagsInput{
		Resources: awsCluster.InstanceIds,
		Tags:      tags,
	}

	if _, err := ec2Svc.CreateTags(tagParams); err != nil {
		return errors.New("Unable to create tags for new instances: " + err.Error())
	}

	describeInstanceStatusInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds: awsCluster.InstanceIds,
	}

	log.Infof("Waitng for %d EC2 instances to be status ok", nodeCount)
	if err := ec2Svc.WaitUntilInstanceStatusOk(describeInstanceStatusInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
	}

	_, err = autoscalingSvc.AttachInstances(&autoscaling.AttachInstancesInput{
		AutoScalingGroupName: deployer.AutoScalingGroup.AutoScalingGroupName,
		InstanceIds:          awsCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to attach new instances to autoscaling group: " + err.Error())
	}

	return nil
}

// CreateDeployment start a deployment
func (deployer *InClusterK8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	awsCluster := deployer.AWSCluster
	deployment := deployer.Deployment
	bastionIp := deployer.BastionIp
	log := deployer.GetLog().Logger

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return nil, fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	autoscalingSvc := autoscaling.New(sess)
	ec2Svc := ec2.New(sess)

	log.Infof("Launching EC2 instances")
	if err := setupEC2(deployer, ec2Svc, autoscalingSvc); err != nil {
		// deployer.DeleteDeployment()
		return nil, errors.New("Unable to setup EC2: " + err.Error())
	}

	if err := populateInClusterNodeInfos(ec2Svc, awsCluster); err != nil {
		return nil, errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := uploadFiles(awsCluster, deployment, uploadedFiles, bastionIp, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, awsCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := deployer.deployKubernetesObjects(k8sClient, false); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return nil, errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	return nil, nil
}

func (deployer *InClusterK8SDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset, skipDelete bool) error {
	namespaces, namespacesErr := getExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		if !skipDelete {
			deleteInClusterDeploymentOnFailure(deployer)
		}
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := createInClusterSecrets(k8sClient, namespaces, deployer.Deployment); err != nil {
		if !skipDelete {
			deleteInClusterDeploymentOnFailure(deployer)
		}
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := deployer.deployServices(k8sClient, namespaces); err != nil {
		if !skipDelete {
			deleteInClusterDeploymentOnFailure(deployer)
		}
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) deployServices(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
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
		family := task.Family
		namespace := createUniqueNamespace(deployment.Name)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

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

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := createServiceForInClusterDeployment(namespace, family, k8sClient, task, container, log)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		deploy := k8sClient.Extensions().Deployments(namespace)
		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unable to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		if task.Deployment != nil {
			return fmt.Errorf("Cannot assign both daemonset and deployment to the same task: %s", task.Family)
		}

		daemonSet := task.DaemonSet
		namespace := createUniqueNamespace(deployment.Name)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	return nil
}

func getInClusterNamespaces(awsCluster *hpaws.AWSCluster) []string {
	return []string{strings.ToLower(awsCluster.Name)}
}

func createUniqueNamespace(deploymentName string) string {
	return strings.ToLower(deploymentName)
}

func createInClusterSecrets(k8sClient *k8s.Clientset, existingNamespaces map[string]bool, deployment *apis.Deployment) error {
	secrets := deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	for _, secret := range secrets {
		namespace := createUniqueNamespace(deployment.Name)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
		}

		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func createServiceForInClusterDeployment(namespace string, family string, k8sClient *k8s.Clientset,
	task apis.KubernetesTask, container v1.Container, log *logging.Logger) error {
	if len(container.Ports) == 0 {
		return nil
	}

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

	i := 1
	for _, reservation := range describeInstancesOutput.Reservations {
		for _, instance := range reservation.Instances {
			nodeInfo := &hpaws.NodeInfo{
				Instance:  instance,
				PrivateIp: *instance.PrivateIpAddress,
			}
			awsCluster.NodeInfos[i] = nodeInfo
			i += 1
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

	if err := deleteK8S(getInClusterNamespaces(deployer.AWSCluster), deployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

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
	log := deployer.GetLog().Logger
	err := deleteK8S(getInClusterNamespaces(deployer.AWSCluster), deployer.KubeConfig, log)
	if err != nil {
		return errors.New("Unable to delete kubernetes objects: " + err.Error())
	}

	sess, sessionErr := hpaws.CreateSession(deployer.AWSCluster.AWSProfile, deployer.AWSCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	autoscalingSvc := autoscaling.New(sess)
	_, err = autoscalingSvc.DetachInstances(&autoscaling.DetachInstancesInput{
		AutoScalingGroupName:           deployer.AutoScalingGroup.AutoScalingGroupName,
		InstanceIds:                    deployer.AWSCluster.InstanceIds,
		ShouldDecrementDesiredCapacity: aws.Bool(true),
	})

	if err != nil {
		return errors.New("Unable to detach instances: " + err.Error())
	}

	_, err = ec2Svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: deployer.AWSCluster.InstanceIds,
	})

	if err != nil {
		return fmt.Errorf("Unable to terminate EC2 instance: %s", err.Error())
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

	awsCluster := deployer.AWSCluster
	for _, node := range awsCluster.NodeInfos {
		awsCluster.InstanceIds = append(awsCluster.InstanceIds, node.Instance.InstanceId)
	}

	autoscalingSvc := autoscaling.New(sess)
	if err := deployer.findAutoscalingGroup(autoscalingSvc); err != nil {
		return errors.New("Unable to find autoscaling group: " + err.Error())
	}

	return nil
}

func (deployer *InClusterK8SDeployer) GetStoreInfo() interface{} {
	return nil
}

func (deployer *InClusterK8SDeployer) GetAWSCluster() *hpaws.AWSCluster {
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
	return "", nil
}

func (deployer *InClusterK8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	return nil, nil
}
