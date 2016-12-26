package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
)

// DeployedCluster stores the data of a cluster
type DeployedCluster struct {
	Name              string
	KeyPair           *ec2.CreateKeyPairOutput
	Deployment        *Deployment
	SecurityGroupId   string
	SubnetId          string
	InternetGatewayId string
	InstanceIds       map[int]string
}

func (deployedCluster *DeployedCluster) KeyName() string {
	return deployedCluster.Deployment.Name + "-key"
}

func (deployedCluster *DeployedCluster) PolicyName() string {
	return deployedCluster.Deployment.Name + "-policy"
}

func (deployedCluster *DeployedCluster) RoleName() string {
	return deployedCluster.Deployment.Name + "-role"
}

func createTags(ec2Svc *ec2.EC2, resources []*string, tags []*ec2.Tag) error {
	tagParams := &ec2.CreateTagsInput{
		Resources: resources,
		Tags:      tags,
	}

	if _, err := ec2Svc.CreateTags(tagParams); err != nil {
		return err
	}

	return nil
}

func createTag(ec2Svc *ec2.EC2, resources []*string, key string, value string) error {
	tags := []*ec2.Tag{
		&ec2.Tag{
			Key:   &key,
			Value: &value,
		},
	}
	return createTags(ec2Svc, resources, tags)
}

func setupECS(deployment *Deployment, ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	// FIXME check if the cluster exists or not
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String(deployment.Name),
	}

	if _, err := ecsSvc.CreateCluster(clusterParams); err != nil {
		return errors.New("Failed to create cluster: " + err.Error())
	}

	for _, taskDefinition := range deployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			DeleteDeployment(deployedCluster)
			return errors.New("Unable to register task definition: " + err.Error())
		}
	}

	return nil
}

func setupNetwork(deployment *Deployment, ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	glog.V(1).Infoln("Creating VPC")
	createVpcInput := &ec2.CreateVpcInput{
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	var vpcId = ""
	if createVpcResponse, err := ec2Svc.CreateVpc(createVpcInput); err != nil {
		return errors.New("Unable to create VPC: " + err.Error())
	} else {
		vpcId = *createVpcResponse.Vpc.VpcId
	}

	vpcAttributeParams := &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcId),
		EnableDnsSupport: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Support with VPC attribute: " + err.Error())
	}

	vpcAttributeParams = &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcId),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Hostname with VPC attribute: " + err.Error())
	}

	glog.V(1).Infoln("Tagging VPC with deployment name")
	if err := createTag(ec2Svc, []*string{&vpcId}, "Name", deployment.Name); err != nil {
		return errors.New("Unable to tag VPC: " + err.Error())
	}

	createSubnetInput := &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcId),
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	if subnetResponse, err := ec2Svc.CreateSubnet(createSubnetInput); err != nil {
		return errors.New("Unable to create subnet: " + err.Error())
	} else {
		deployedCluster.SubnetId = *subnetResponse.Subnet.SubnetId
	}

	if gatewayResponse, err := ec2Svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}); err != nil {
		return errors.New("Unable to create internet gateway: " + err.Error())
	} else {
		deployedCluster.InternetGatewayId = *gatewayResponse.InternetGateway.InternetGatewayId
	}

	if err := createTag(ec2Svc, []*string{&deployedCluster.InternetGatewayId}, "Name", deployment.Name); err != nil {
		return errors.New("Unable to tag internet gateway: " + err.Error())
	}

	attachInternetInput := &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(deployedCluster.InternetGatewayId),
		VpcId:             aws.String(vpcId),
	}

	if _, err := ec2Svc.AttachInternetGateway(attachInternetInput); err != nil {
		return errors.New("Unable to attach internet gateway: " + err.Error())
	}

	var routeTableId = ""
	if describeRoutesOutput, err := ec2Svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{}); err != nil {
		return errors.New("Unable to describe route tables: " + err.Error())
	} else {
		for _, routeTable := range describeRoutesOutput.RouteTables {
			if *routeTable.VpcId == vpcId {
				routeTableId = *routeTable.RouteTableId
				break
			}
		}
	}

	if routeTableId == "" {
		return errors.New("Unable to find route table associated with vpc")
	}

	createRouteInput := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(routeTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(deployedCluster.InternetGatewayId),
	}

	if createRouteOutput, err := ec2Svc.CreateRoute(createRouteInput); err != nil {
		return errors.New("Unable to create route: " + err.Error())
	} else if !*createRouteOutput.Return {
		return errors.New("Failed to create route")
	}

	securityGroupParams := &ec2.CreateSecurityGroupInput{
		Description: aws.String(deployment.Name), // Required
		GroupName:   aws.String(deployment.Name), // Required
		VpcId:       aws.String(vpcId),
	}
	glog.V(1).Infoln("Creating security group")
	securityGroupResp, err := ec2Svc.CreateSecurityGroup(securityGroupParams)
	if err != nil {
		return errors.New("Unable to create security group: " + err.Error())
	}

	deployedCluster.SecurityGroupId = *securityGroupResp.GroupId

	ports := make(map[int]string)
	for _, port := range deployment.AllowedPorts {
		ports[port] = "tcp"
	}
	// Open http and https by default
	ports[22] = "tcp"
	ports[80] = "tcp"

	// Also ports needed by Weave
	ports[6783] = "tcp,udp"
	ports[6784] = "udp"

	glog.V(1).Infoln("Allowing ingress input")
	for port, protocols := range ports {
		for _, protocol := range strings.Split(protocols, ",") {
			securityGroupIngressParams := &ec2.AuthorizeSecurityGroupIngressInput{
				CidrIp:     aws.String("0.0.0.0/0"),
				FromPort:   aws.Int64(int64(port)),
				ToPort:     aws.Int64(int64(port)),
				GroupId:    securityGroupResp.GroupId,
				IpProtocol: aws.String(protocol),
			}

			_, err = ec2Svc.AuthorizeSecurityGroupIngress(securityGroupIngressParams)
			if err != nil {
				return errors.New("Unable to authorize security group ingress: " + err.Error())
			}
		}
	}

	return nil
}

func setupEC2(deployment *Deployment, sess *session.Session, deployedCluster *DeployedCluster) error {
	ec2Svc := ec2.New(sess)

	if err := setupNetwork(deployment, ec2Svc, deployedCluster); err != nil {
		return err
	}

	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployedCluster.KeyName()),
	}
	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		DeleteDeployment(deployedCluster)
		return errors.New("Unable to create key pair: " + keyErr.Error())
	}

	deployedCluster.KeyPair = keyOutput
	userData := base64.StdEncoding.EncodeToString([]byte(
		fmt.Sprintf(`#!/bin/bash
echo ECS_CLUSTER=%s >> /etc/ecs/ecs.config
weave launch`, deployment.Name)))

	associatePublic := true
	for _, node := range deployment.ClusterDefinition.Nodes {
		runResult, runErr := ec2Svc.RunInstances(&ec2.RunInstancesInput{
			KeyName: aws.String(*keyOutput.KeyName),
			ImageId: aws.String(node.ImageId),
			NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{
				&ec2.InstanceNetworkInterfaceSpecification{
					AssociatePublicIpAddress: &associatePublic,
					DeleteOnTermination:      &associatePublic,
					DeviceIndex:              aws.Int64(0),
					Groups:                   []*string{&deployedCluster.SecurityGroupId},
					SubnetId:                 aws.String(deployedCluster.SubnetId),
				},
			},
			InstanceType: aws.String(node.InstanceType),
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Name: aws.String(deployment.Name),
			},
			MinCount: aws.Int64(1),
			MaxCount: aws.Int64(1),
			UserData: aws.String(userData),
		})

		if runErr != nil {
			DeleteDeployment(deployedCluster)
			return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
		}

		deployedCluster.InstanceIds[node.Id] = *runResult.Instances[0].InstanceId
	}

	ids := make([]*string, 0, len(deployedCluster.InstanceIds))

	for _, value := range deployedCluster.InstanceIds {
		ids = append(ids, &value)
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String("Deployment"),
			Value: aws.String(deployment.Name),
		},
		{
			Key:   aws.String("weave:peerGroupName"),
			Value: aws.String(deployment.Name),
		},
	}

	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: ids,
	}

	glog.V(1).Infof("Waitng for %d EC2 instances to exist", len(ids))
	if err := ec2Svc.WaitUntilInstanceExists(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	if err := createTags(ec2Svc, ids, tags); err != nil {
		DeleteDeployment(deployedCluster)
		return errors.New("Unable to create tags for instances: " + err.Error())
	}

	glog.V(1).Infof("Waitng for %d EC2 instances to launch", len(ids))
	if err := ec2Svc.WaitUntilInstanceRunning(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be running: " + err.Error())
	}

	return nil
}

func setupIAM(deployment *Deployment, sess *session.Session, deployedCluster *DeployedCluster) error {
	iamSvc := iam.New(sess)

	roleParams := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(trustDocument),
		RoleName:                 aws.String(deployedCluster.RoleName()),
	}
	// create IAM role
	if _, err := iamSvc.CreateRole(roleParams); err != nil {
		glog.Errorf("Failed to create IAM role: %s", err)
		DeleteDeployment(deployedCluster)
		return err
	}

	var policyDocument *string
	if deployment.IamRole.PolicyDocument != "" {
		policyDocument = aws.String(deployment.IamRole.PolicyDocument)
	} else {
		policyDocument = aws.String(defaultRolePolicy)
	}

	// create role policy
	rolePolicyParams := &iam.PutRolePolicyInput{
		RoleName:       aws.String(deployedCluster.RoleName()),
		PolicyName:     aws.String(deployedCluster.PolicyName()),
		PolicyDocument: policyDocument,
	}

	if _, err := iamSvc.PutRolePolicy(rolePolicyParams); err != nil {
		glog.Errorf("Failed to put role policy: %s", err)
		DeleteDeployment(deployedCluster)
		return err
	}

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(deployment.Name),
	}

	if _, err := iamSvc.CreateInstanceProfile(iamParams); err != nil {
		glog.Errorf("Failed to create instance profile: %s", err)
		DeleteDeployment(deployedCluster)
		return err
	}

	roleInstanceProfileParams := &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(deployment.Name),
		RoleName:            aws.String(deployedCluster.RoleName()),
	}

	if _, err := iamSvc.AddRoleToInstanceProfile(roleInstanceProfileParams); err != nil {
		glog.Errorf("Failed to add role to instance profile: %s", err)
		DeleteDeployment(deployedCluster)
		return err
	}

	return nil
}

func setupAutoScaling(deployment *Deployment, sess *session.Session, deployedCluster *DeployedCluster) error {
	svc := autoscaling.New(sess)

	userData := base64.StdEncoding.EncodeToString([]byte(
		`#!/bin/bash
echo ECS_CLUSTER=` + deployment.Name + " >> /etc/ecs/ecs.config"))

	params := &autoscaling.CreateLaunchConfigurationInput{
		LaunchConfigurationName:  aws.String(deployment.Name), // Required
		AssociatePublicIpAddress: aws.Bool(true),
		EbsOptimized:             aws.Bool(true),
		IamInstanceProfile:       aws.String(deployment.Name),
		ImageId:                  aws.String(amiCollection[deployment.Region]),
		InstanceMonitoring: &autoscaling.InstanceMonitoring{
			Enabled: aws.Bool(false),
		},
		InstanceType: aws.String("t2.medium"),
		KeyName:      aws.String(deployment.Name),
		SecurityGroups: []*string{
			&deployedCluster.SecurityGroupId,
		},
		UserData: aws.String(userData),
	}
	_, err := svc.CreateLaunchConfiguration(params)

	if err != nil {
		return err
	}

	autoScalingGroupParams := &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName:    aws.String(deployment.Name),
		MaxSize:                 aws.Int64(deployment.Scale),
		MinSize:                 aws.Int64(deployment.Scale),
		DefaultCooldown:         aws.Int64(1),
		DesiredCapacity:         aws.Int64(deployment.Scale),
		LaunchConfigurationName: aws.String(deployment.Name),
		// NOTE this fiedl is required once we have the function of setupVpc
		// VPCZoneIdentifier: aws.String("XmlStringMaxLen2047"),
	}

	if _, err = svc.CreateAutoScalingGroup(autoScalingGroupParams); err != nil {
		return err
	}
	return nil
}

func errorMessageFromFailures(failures []*ecs.Failure) string {
	var failureMessage = ""
	for _, failure := range failures {
		failureMessage += *failure.Reason + ", "
	}

	return failureMessage
}

func launchECSTasks(deployment *Deployment, ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	// Wait for ECS instances to be ready
	clusterReady := false
	totalCount := int64(len(deployment.ClusterDefinition.Nodes))
	var describeOutput *ecs.DescribeClustersOutput
	var err error
	for !clusterReady {
		describeOutput, err = ecsSvc.DescribeClusters(&ecs.DescribeClustersInput{
			Clusters: []*string{&deployment.Name},
		})

		if err != nil {
			return errors.New("Unable to list ECS clusters: " + err.Error())
		}

		if len(describeOutput.Failures) > 0 {
			return errors.New(
				"Failed to list ECS clusters: " + errorMessageFromFailures(describeOutput.Failures))
		}

		registeredCount := *describeOutput.Clusters[0].RegisteredContainerInstancesCount

		if registeredCount == totalCount {
			break
		}

		glog.Infof("Cluster not ready. registered %d, total %v", registeredCount, totalCount)
		time.Sleep(time.Duration(3) * time.Second)
	}

	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(deployment.Name),
	}

	var containerInstances []*string
	nodeIdToArn := make(map[int]string)
	if listInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput); err != nil {
		return errors.New("Failed to list container instances: " + err.Error())
	} else {
		containerInstances = listInstancesOutput.ContainerInstanceArns
	}

	describeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deployment.Name),
		ContainerInstances: containerInstances,
	}

	if describeInstancesOutput, err := ecsSvc.DescribeContainerInstances(describeInstancesInput); err != nil {
		return errors.New("Failed to describe container instances: " + err.Error())
	} else {
		for _, instance := range describeInstancesOutput.ContainerInstances {
			for key, value := range deployedCluster.InstanceIds {
				if *instance.Ec2InstanceId == value {
					nodeIdToArn[key] = *instance.ContainerInstanceArn
				}
			}
		}
	}

	glog.V(1).Infof("Node mapping: %v", deployment.NodeMapping)
	for _, mapping := range deployment.NodeMapping {
		instanceId, ok := nodeIdToArn[mapping.Id]
		if !ok {
			err := fmt.Sprintf("Unable to find Node id %d in instance map", mapping.Id)
			glog.Error(err)
			DeleteDeployment(deployedCluster)
			return errors.New(err)
		}

		glog.Infof("Starting task %s on node %d", mapping.Task, mapping.Id)
		startTaskOutput, err := ecsSvc.StartTask(&ecs.StartTaskInput{
			Cluster:            aws.String(deployment.Name),
			TaskDefinition:     aws.String(mapping.Task),
			ContainerInstances: []*string{&instanceId},
		})

		if err != nil {
			startTaskErr := fmt.Sprintf("Unable to start task %v\nError: %v", mapping.Task, err)
			DeleteDeployment(deployedCluster)
			return errors.New(startTaskErr)
		}

		if len(startTaskOutput.Failures) > 0 {
			errorMessage := fmt.Sprintf(
				"Failed to start task %v\nMessage: %v",
				mapping.Task,
				errorMessageFromFailures(startTaskOutput.Failures))
			glog.Errorf(errorMessage)
			DeleteDeployment(deployedCluster)
			return errors.New(errorMessage)
		}
	}

	return nil
}

func NewDeployedCluster(deployment *Deployment) *DeployedCluster {
	return &DeployedCluster{
		Name:        deployment.Name,
		Deployment:  deployment,
		InstanceIds: make(map[int]string),
	}
}

// CreateDeployment start a deployment
func CreateDeployment(viper *viper.Viper, deployment *Deployment, deployedCluster *DeployedCluster) error {
	awsId := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(deployment.Region),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Failed to create session: %s", err)
		return err
	}
	ecsSvc := ecs.New(sess)
	if err = setupECS(deployment, ecsSvc, deployedCluster); err != nil {
		return errors.New("Failed to setup ECS: " + err.Error())
	}

	if err = setupIAM(deployment, sess, deployedCluster); err != nil {
		return errors.New("Failed to setup IAM: " + err.Error())
	}

	//if err = setupAutoScaling(deployment, sess, deployedCluster); err != nil {
	//	return nil, err
	//}

	if err = setupEC2(deployment, sess, deployedCluster); err != nil {
		return errors.New("Failed to setup EC2: " + err.Error())
	}
	if err = launchECSTasks(deployment, ecsSvc, deployedCluster); err != nil {
		return errors.New("Failed to launch ECS tasks: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from AWS ECS.
func DeleteDeployment(deployedCluster *DeployedCluster) error {
	// TODO check region of the deployment is defined
	// TODO stop all the ecs tasks
	// TODO delete all the task definitions
	// NOTE if we create autoscaling, delete it. Wait until the deletes all the instance.
	// Delete the launch configuration
	// TODO delete IAM role
	// TODO delete key pair
	// TODO delete security group
	// NOTE we don't have internet gateway and subnet. We can ignore them until we have them.
	// TODO Delete vpc
	// TODO Delete ecs cluster
	return nil

}
