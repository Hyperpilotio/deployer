package kubernetes

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/spf13/viper"

	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"k8s.io/client-go/rest"
)

type InClusterK8SDeployer struct {
	K8SDeployer

	OriginalAWSCluster *hpaws.AWSCluster
	AutoScalingGroup   *autoscaling.Group
}

func NewInClusterDeployer(
	config *viper.Viper,
	awsProfile *hpaws.AWSProfile,
	deployment *apis.Deployment,
	originalAWSCluster *hpaws.AWSCluster,
	kubeConfig *rest.Config) (*InClusterK8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	awsCluster := hpaws.NewAWSCluster(originalAWSCluster.Name, originalAWSCluster.Region, awsProfile)
	deployer := &InClusterK8SDeployer{
		K8SDeployer: K8SDeployer{
			Config:        config,
			AWSCluster:    awsCluster,
			Deployment:    deployment,
			DeploymentLog: log,
			Services:      make(map[string]ServiceMapping),
			KubeConfig:    kubeConfig,
		},
		OriginalAWSCluster: originalAWSCluster,
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
			if *tag.Key == "KubernetesCluster" && *tag.Value == deployer.AWSCluster.StackName() {
				autoScalingGroup = group
				break
			}
		}
	}

	if autoScalingGroup == nil {
		return errors.New("Unable to find auto scaling group for stack: " + deployer.AWSCluster.StackName())
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

// CreateDeployment start a deployment
func (deployer *InClusterK8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	awsCluster := deployer.AWSCluster
	// log := deployer.DeploymentLog.Logger

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return nil, fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	autoscalingSvc := autoscaling.New(sess)
	ec2Svc := ec2.New(sess)
	if err := deployer.findAutoscalingGroup(autoscalingSvc); err != nil {
		return nil, errors.New("Unable to find autoscaling group: " + err.Error())
	}

	launchConfig, err := deployer.getLaunchConfiguration(autoscalingSvc)
	if err != nil {
		return nil, errors.New("Unable to get launch configuration: " + err.Error())
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
					aws.String(awsCluster.StackName()),
				},
			},
		},
	}
	describeInstancesOutput, describeErr := ec2Svc.DescribeInstances(describeInstancesInput)
	if describeErr != nil {
		return nil, errors.New("Unable to describe ec2 instances: " + describeErr.Error())
	}

	if len(describeInstancesOutput.Reservations) == 0 || len(describeInstancesOutput.Reservations[0].Instances) == 0 {
		return nil, errors.New("Unable to find any running k8s node in cluster")
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
				// DeleteOnTermination: mapping.Ebs.DeleteOnTermination,
				// Encrypted:           mapping.Ebs.Encrypted,
				// Iops:                mapping.Ebs.Iops,
				// SnapshotId:          mapping.Ebs.SnapshotId,
				VolumeSize: mapping.Ebs.VolumeSize,
				VolumeType: mapping.Ebs.VolumeType,
			},
			// NoDevice:    aws.String(strconv.FormatBool(*mapping.NoDevice)),
			// VirtualName: mapping.VirtualName,
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
		// SecurityGroups: launchConfig.SecurityGroups,
		UserData: launchConfig.UserData,
		// KernelId:          launchConfig.KernelId,
		NetworkInterfaces: networkSpecs,
		InstanceType:      aws.String(node.InstanceType),
		MinCount:          aws.Int64(1),
		MaxCount:          aws.Int64(1),
	}
	glog.V(1).Infof("runInstancesInput: %+v", runInstancesInput)

	runResult, runErr := ec2Svc.RunInstances(runInstancesInput)
	if runErr != nil {
		return nil, errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
	}

	if len(runResult.Instances) != nodeCount {
		return nil, fmt.Errorf("Unable to find equal amount of nodes after ec2 create, expecting: %d, found: %d",
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
		return nil, errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String("Name"),
			Value: aws.String("k8s-node"),
		},
		{
			Key:   aws.String("KubernetesCluster"),
			Value: aws.String(awsCluster.StackName()),
		},
	}

	tagParams := &ec2.CreateTagsInput{
		Resources: awsCluster.InstanceIds,
		Tags:      tags,
	}

	if _, err := ec2Svc.CreateTags(tagParams); err != nil {
		return nil, errors.New("Unable to create tags for new instances: " + err.Error())
	}

	return nil, nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (inK8SDeployer *InClusterK8SDeployer) UpdateDeployment() error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (inK8SDeployer *InClusterK8SDeployer) DeleteDeployment() error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetStoreInfo() interface{} {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetAWSCluster() *hpaws.AWSCluster {
	return inK8SDeployer.AWSCluster
}

func (inK8SDeployer *InClusterK8SDeployer) GetLog() *log.FileLog {
	return inK8SDeployer.DeploymentLog
}

func (inK8SDeployer *InClusterK8SDeployer) GetScheduler() *job.Scheduler {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	return "", nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	return nil, nil
}
