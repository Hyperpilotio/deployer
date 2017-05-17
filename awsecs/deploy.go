package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/common"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	"github.com/spf13/viper"
)

// NewDeployer return the EC2 of Deployer
func NewDeployer(config *viper.Viper, awsProfiles map[string]*AWSProfile,
	deployment *apis.Deployment) (*ECSDeployer, error) {
	deployedCluster := NewDeployedCluster(deployment)
	deployer := &ECSDeployer{
		Config: config,
		Type:   "ECS",
		DeploymentInfo: &DeploymentInfo{
			AwsInfo: deployedCluster,
			Created: time.Now(),
			State:   CREATING,
		},
	}

	deployer.mutex.Lock()
	defer deployer.mutex.Unlock()

	awsProfile, ok := awsProfiles[deployment.UserId]
	if !ok {
		return nil, errors.New("Unable to find aws profile for user: " + deployment.UserId)
	}
	deployer.DeploymentInfo.AwsProfile = awsProfile

	log, err := log.NewLogger(config, deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	deployer.DeploymentLog = log
	deployer.DeploymentInfo.AwsInfo.Logger = log.Logger

	return deployer, nil
}

// NewDeployedCluster return the instance of DeployedCluster
func NewDeployedCluster(deployment *apis.Deployment) *DeployedCluster {
	return &DeployedCluster{
		Name:        deployment.Name,
		Deployment:  deployment,
		NodeInfos:   make(map[int]*NodeInfo),
		InstanceIds: make([]*string, 0),
	}
}

// NewStoreDeployment create deployment that needs to be stored
func (deploymentInfo *DeploymentInfo) NewStoreDeployment() *StoreDeployment {
	storeDeployment := &StoreDeployment{
		Name:    deploymentInfo.AwsInfo.Deployment.Name,
		Region:  deploymentInfo.AwsInfo.Deployment.Region,
		UserId:  deploymentInfo.AwsInfo.Deployment.UserId,
		Status:  GetStateString(deploymentInfo.State),
		Created: deploymentInfo.Created.Format(time.RFC822),
	}

	if deploymentInfo.AwsInfo.KeyPair != nil {
		storeDeployment.KeyMaterial = aws.StringValue(deploymentInfo.AwsInfo.KeyPair.KeyMaterial)
	}

	deploymentType := deploymentInfo.GetDeploymentType()
	storeDeployment.Type = deploymentType
	switch deploymentType {
	case "K8S":
		storeDeployment.K8SDeployment = deploymentInfo.K8sInfo.NewK8SStoreDeployment()
	}

	return storeDeployment
}

func (k8sDeployment *KubernetesDeployment) NewK8SStoreDeployment() *K8SStoreDeployment {
	return &K8SStoreDeployment{
		BastionIp: k8sDeployment.BastionIp,
		MasterIp:  k8sDeployment.MasterIp,
	}
}

func (ecsDeployer *ECSDeployer) GetDeploymentInfo() *DeploymentInfo {
	return ecsDeployer.DeploymentInfo
}

func (ecsDeployer *ECSDeployer) GetLog() *log.DeploymentLog {
	return ecsDeployer.DeploymentLog
}

func (ecsDeployer *ECSDeployer) GetScheduler() *job.Scheduler {
	return ecsDeployer.Scheduler
}

func (ecsDeployer *ECSDeployer) GetKubeConfigPath() string {
	return ""
}

func (ecsDeployer *ECSDeployer) NewShutDownScheduler() error {
	if ecsDeployer.Scheduler != nil {
		ecsDeployer.Scheduler.Stop()
	}

	deployment := ecsDeployer.DeploymentInfo.AwsInfo.Deployment

	scheduleRunTime := ""
	if deployment.ShutDownTime != "" {
		scheduleRunTime = deployment.ShutDownTime
	} else {
		scheduleRunTime = ecsDeployer.Config.GetString("shutDownTime")
	}

	startTime, err := time.ParseDuration(scheduleRunTime)
	if err != nil {
		return fmt.Errorf("Unable to parse shutDownTime %s: %s", scheduleRunTime, err.Error())
	}
	glog.Infof("New %s schedule at %s", deployment.Name, time.Now().Add(startTime))

	scheduler, scheduleErr := job.NewScheduler(deployment.Name, startTime, func() {
		go func() {
			ecsDeployer.DeleteDeployment()
		}()
	})

	if scheduleErr != nil {
		return fmt.Errorf("Unable to schedule %s to auto shutdown cluster: %s", deployment.Name, scheduleErr.Error())
	}
	ecsDeployer.Scheduler = scheduler

	return nil
}

// CreateDeployment start a deployment
func (ecsDeployer *ECSDeployer) CreateDeployment(uploadedFiles map[string]string) error {
	deploymentInfo := ecsDeployer.DeploymentInfo
	deployedCluster := deploymentInfo.AwsInfo
	deployment := deployedCluster.Deployment
	awsProfile := deploymentInfo.AwsProfile
	log := deployedCluster.Logger

	sess, sessionErr := CreateSession(awsProfile, deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ecsSvc := ecs.New(sess)
	ec2Svc := ec2.New(sess)
	iamSvc := iam.New(sess)

	log.Infof("Creating AWS Log Group")
	if err := setupAWSLogsGroup(sess, deployment); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup AWS Log Group for container: " + err.Error())
	}

	log.Infof("Setting up ECS cluster")
	if err := setupECS(ecsSvc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup ECS: " + err.Error())
	}

	if err := ecsDeployer.SetupEC2Infra("ec2-user", uploadedFiles, ec2Svc, iamSvc, ecsAmis); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup EC2: " + err.Error())
	}

	log.Infof("Waiting for ECS cluster to be ready")
	if err := waitUntilECSClusterReady(ecsSvc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to wait until ECS cluster ready: " + err.Error())
	}

	log.Infof("Add attribute on ECS instances")
	if err := setupInstanceAttribute(ecsSvc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup instance attribute: " + err.Error())
	}

	log.Infof("Launching ECS services")
	if err := createServices(ecsSvc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to launch ECS tasks: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from AWS ECS.
func (ecsDeployer *ECSDeployer) DeleteDeployment() error {
	deploymentInfo := ecsDeployer.DeploymentInfo
	deployedCluster := deploymentInfo.AwsInfo
	deployment := deployedCluster.Deployment
	awsProfile := deploymentInfo.AwsProfile
	log := deployedCluster.Logger

	sess, sessionErr := CreateSession(awsProfile, deployment)
	if sessionErr != nil {
		log.Errorf("Unable to create session: %s" + sessionErr.Error())
		return sessionErr
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	log.Infof("Checking VPC for deletion")
	if err := checkVPC(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to find VPC: %s", err.Error())
		return err
	}

	if deployedCluster.Deployment.ECSDeployment != nil {
		// Stop all running tasks
		log.Infof("Stopping all ECS services")
		if err := stopECSServices(ecsSvc, deployedCluster); err != nil {
			log.Errorf("Unable to stop ECS services: ", err.Error())
			return err
		}

		// delete all the task definitions
		log.Infof("Deleting task definitions")
		if err := deleteTaskDefinitions(ecsSvc, deployedCluster); err != nil {
			log.Errorf("Unable to delete task definitions: %s", err.Error())
			return err
		}
	}

	// Terminate EC2 instance
	log.Infof("Deleting EC2 instances")
	if err := deleteEC2(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete task definitions: %s", err.Error())
		return err
	}

	// NOTE if we create autoscaling, delete it. Wait until the deletes all the instance.
	// Delete the launch configuration

	// delete IAM role
	iamSvc := iam.New(sess)

	log.Infof("Deleting IAM role")
	if err := deleteIAM(iamSvc, deployedCluster); err != nil {
		log.Errorf("Unable to delete IAM: %s", err.Error())
		return err
	}

	// delete key pair
	log.Infof("Deleting key pair")
	if err := DeleteKeyPair(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete key pair: %s", err.Error())
		return err
	}

	// delete security group
	log.Infof("Deleting security group")
	if err := deleteSecurityGroup(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete security group: %s", err.Error())
		return err
	}

	// delete internet gateway.
	log.Infof("Deleting internet gateway")
	if err := deleteInternetGateway(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete internet gateway: %s", err.Error())
		return err
	}

	// delete subnet.
	log.Infof("Deleting subnet")
	if err := deleteSubnet(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete subnet: %s", err.Error())
		return err
	}

	// Delete VPC
	log.Infof("Deleting VPC")
	if err := deleteVPC(ec2Svc, deployedCluster); err != nil {
		log.Errorf("Unable to delete VPC: %s", err)
		return err
	}

	if deployedCluster.Deployment.ECSDeployment != nil {
		// Delete ecs cluster
		log.Infof("Deleting ECS cluster")
		if err := deleteCluster(ecsSvc, deployedCluster); err != nil {
			log.Errorf("Unable to delete ECS cluster: %s", err)
			return err
		}
	}

	return nil
}

// KeyName return a key name according to the Deployment.Name with suffix "-key"
func (deployedCluster *DeployedCluster) KeyName() string {
	return deployedCluster.Deployment.Name + "-key"
}

func (deployedCluster *DeployedCluster) StackName() string {
	return deployedCluster.Deployment.Name + "-stack"
}

// PolicyName return a key name according to the Deployment.Name with suffix "-policy"
func (deployedCluster *DeployedCluster) PolicyName() string {
	return deployedCluster.Deployment.Name + "-policy"
}

// RoleName return a key name according to the Deployment.Name with suffix "-role"
func (deployedCluster *DeployedCluster) RoleName() string {
	return deployedCluster.Deployment.Name + "-role"
}

// VPCName return a key name according to the Deployment.Name with suffix "-vpc"
func (deployedCluster DeployedCluster) VPCName() string {
	return deployedCluster.Name + "-vpc"
}

// SubnetName return a key name according to the Deployment.Name with suffix "-vpc"
func (deployedCluster DeployedCluster) SubnetName() string {
	return deployedCluster.Name + "-subnet"
}

func (deployedCluster DeployedCluster) SshConfig(user string) (*ssh.ClientConfig, error) {
	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}

	return clientConfig, nil
}

func CreateSession(awsProfile *AWSProfile, deployment *apis.Deployment) (*session.Session, error) {
	awsId := awsProfile.AwsId
	awsSecret := awsProfile.AwsSecret
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(deployment.Region),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Unable to create session: %s", err)
		return nil, err
	}

	return sess, nil
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

func setupECS(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	// FIXME check if the cluster exists or not
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String(deployedCluster.Deployment.Name),
	}

	if _, err := ecsSvc.CreateCluster(clusterParams); err != nil {
		return errors.New("Unable to create cluster: " + err.Error())
	}

	for _, taskDefinition := range deployedCluster.Deployment.ECSDeployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			return errors.New("Unable to register task definition: " + err.Error())
		}
	}

	return nil
}

func setupNetwork(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	log.Infof("Creating VPC")
	createVpcInput := &ec2.CreateVpcInput{
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	if createVpcResponse, err := ec2Svc.CreateVpc(createVpcInput); err != nil {
		return errors.New("Unable to create VPC: " + err.Error())
	} else {
		deployedCluster.VpcId = *createVpcResponse.Vpc.VpcId
	}

	vpcAttributeParams := &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(deployedCluster.VpcId),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Support with VPC attribute: " + err.Error())
	}

	vpcAttributeParams = &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(deployedCluster.VpcId),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err := ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return errors.New("Unable to enable DNS Hostname with VPC attribute: " + err.Error())
	}

	log.Infof("Tagging VPC with deployment name")
	if err := createTag(ec2Svc, []*string{&deployedCluster.VpcId}, "Name", deployedCluster.VPCName()); err != nil {
		return errors.New("Unable to tag VPC: " + err.Error())
	}

	createSubnetInput := &ec2.CreateSubnetInput{
		VpcId:     aws.String(deployedCluster.VpcId),
		CidrBlock: aws.String("172.31.0.0/28"),
	}

	// NOTE Unhandled case:
	// 1. if subnet created successfuly but aws sdk connection broke or request reached timeout
	if subnetResponse, err := ec2Svc.CreateSubnet(createSubnetInput); err != nil {
		return errors.New("Unable to create subnet: " + err.Error())
	} else {
		deployedCluster.SubnetId = *subnetResponse.Subnet.SubnetId
	}

	describeSubnetsInput := &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(deployedCluster.SubnetId)},
	}

	if err := ec2Svc.WaitUntilSubnetAvailable(describeSubnetsInput); err != nil {
		return errors.New("Unable to wait until subnet available: " + err.Error())
	}

	if err := createTag(ec2Svc, []*string{&deployedCluster.SubnetId}, "Name", deployedCluster.SubnetName()); err != nil {
		return errors.New("Unable to tag subnet: " + err.Error())
	}

	if gatewayResponse, err := ec2Svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}); err != nil {
		return errors.New("Unable to create internet gateway: " + err.Error())
	} else {
		deployedCluster.InternetGatewayId = *gatewayResponse.InternetGateway.InternetGatewayId
	}

	// We have to sleep as sometimes the internet gateway won't be available yet. And sadly aws-sdk-go has no function to wait for it.
	time.Sleep(time.Second * 5)

	if err := createTag(ec2Svc, []*string{&deployedCluster.InternetGatewayId}, "Name", deployedCluster.Deployment.Name); err != nil {
		return errors.New("Unable to tag internet gateway: " + err.Error())
	}

	attachInternetInput := &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(deployedCluster.InternetGatewayId),
		VpcId:             aws.String(deployedCluster.VpcId),
	}

	if _, err := ec2Svc.AttachInternetGateway(attachInternetInput); err != nil {
		return errors.New("Unable to attach internet gateway: " + err.Error())
	}

	var routeTableId = ""
	if describeRoutesOutput, err := ec2Svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{}); err != nil {
		return errors.New("Unable to describe route tables: " + err.Error())
	} else {
		for _, routeTable := range describeRoutesOutput.RouteTables {
			if *routeTable.VpcId == deployedCluster.VpcId {
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
		return errors.New("Unable to create route")
	}

	securityGroupParams := &ec2.CreateSecurityGroupInput{
		Description: aws.String(deployedCluster.Deployment.Name),
		GroupName:   aws.String(deployedCluster.Deployment.Name),
		VpcId:       aws.String(deployedCluster.VpcId),
	}
	log.Infof("Creating security group")
	securityGroupResp, err := ec2Svc.CreateSecurityGroup(securityGroupParams)
	if err != nil {
		return errors.New("Unable to create security group: " + err.Error())
	}

	deployedCluster.SecurityGroupId = *securityGroupResp.GroupId

	ports := make(map[int]string)
	for _, port := range deployedCluster.Deployment.AllowedPorts {
		ports[port] = "tcp"
	}
	// Open http and https by default
	ports[22] = "tcp"
	ports[80] = "tcp"

	// Also ports needed by Weave
	ports[6783] = "tcp,udp"
	ports[6784] = "udp"

	log.Infof("Allowing ingress input")
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

func uploadFiles(user string, ec2Svc *ec2.EC2, uploadedFiles map[string]string, deployedCluster *DeployedCluster) error {
	if len(deployedCluster.Deployment.Files) == 0 {
		return nil
	}

	clientConfig, err := deployedCluster.SshConfig(user)
	if err != nil {
		return errors.New("Unable to create ssh config: " + err.Error())
	}

	for _, nodeInfo := range deployedCluster.NodeInfos {
		address := nodeInfo.PublicDnsName + ":22"
		scpClient := common.NewSshClient(address, clientConfig, "")
		for _, deployFile := range deployedCluster.Deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := scpClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, address, err.Error())
				return errors.New(errorMsg)
			}
		}
	}

	return nil
}

func CreateKeypair(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployedCluster.KeyName()),
	}

	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		return errors.New("Unable to create key pair: " + keyErr.Error())
	}

	deployedCluster.KeyPair = keyOutput

	return nil
}

func setupEC2(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster, images map[string]string) error {
	log := deployedCluster.Logger
	if err := CreateKeypair(ec2Svc, deployedCluster); err != nil {
		return err
	}

	userData := base64.StdEncoding.EncodeToString([]byte(
		fmt.Sprintf(`#!/bin/bash
echo ECS_CLUSTER=%s >> /etc/ecs/ecs.config
echo manual > /etc/weave/scope.override
weave launch`, deployedCluster.Deployment.Name)))

	associatePublic := true
	for _, node := range deployedCluster.Deployment.ClusterDefinition.Nodes {
		runResult, runErr := ec2Svc.RunInstances(&ec2.RunInstancesInput{
			KeyName: aws.String(*deployedCluster.KeyPair.KeyName),
			ImageId: aws.String(images[deployedCluster.Deployment.Region]),
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
				Name: aws.String(deployedCluster.Deployment.Name),
			},
			MinCount: aws.Int64(1),
			MaxCount: aws.Int64(1),
			UserData: aws.String(userData),
		})

		if runErr != nil {
			return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
		}

		deployedCluster.NodeInfos[node.Id] = &NodeInfo{
			Instance: runResult.Instances[0],
		}
		deployedCluster.InstanceIds = append(deployedCluster.InstanceIds, runResult.Instances[0].InstanceId)
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String("Deployment"),
			Value: aws.String(deployedCluster.Deployment.Name),
		},
		{
			Key:   aws.String("weave:peerGroupName"),
			Value: aws.String(deployedCluster.Deployment.Name),
		},
	}

	nodeCount := len(deployedCluster.InstanceIds)
	log.Infof("Waitng for %d EC2 instances to exist", nodeCount)

	describeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: deployedCluster.InstanceIds,
	}

	if err := ec2Svc.WaitUntilInstanceExists(describeInstancesInput); err != nil {
		return errors.New("Unable to wait for ec2 instances to exist: " + err.Error())
	}

	// We are trying to tag before it's running as weave requires the tag to function,
	// so the earlier we tag the better chance we have to see the cluster ready in ECS
	if err := createTags(ec2Svc, deployedCluster.InstanceIds, tags); err != nil {
		return errors.New("Unable to create tags for instances: " + err.Error())
	}

	describeInstanceStatusInput := &ec2.DescribeInstanceStatusInput{
		InstanceIds: deployedCluster.InstanceIds,
	}

	log.Infof("Waitng for %d EC2 instances to be status ok", nodeCount)
	if err := ec2Svc.WaitUntilInstanceStatusOk(describeInstanceStatusInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
	}

	return nil
}

func setupIAM(iamSvc *iam.IAM, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	roleParams := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(trustDocument),
		RoleName:                 aws.String(deployedCluster.RoleName()),
	}
	// create IAM role
	if _, err := iamSvc.CreateRole(roleParams); err != nil {
		return errors.New("Unable to create IAM role: " + err.Error())
	}

	var policyDocument *string
	if deployedCluster.Deployment.IamRole.PolicyDocument != "" {
		policyDocument = aws.String(deployedCluster.Deployment.IamRole.PolicyDocument)
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
		log.Errorf("Unable to put role policy: %s", err)
		return err
	}

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Deployment.Name),
	}

	if _, err := iamSvc.CreateInstanceProfile(iamParams); err != nil {
		log.Errorf("Unable to create instance profile: %s", err)
		return err
	}

	getProfileInput := &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Deployment.Name),
	}

	if err := iamSvc.WaitUntilInstanceProfileExists(getProfileInput); err != nil {
		return errors.New("Unable to wait for instance profile to exist: " + err.Error())
	}

	roleInstanceProfileParams := &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Deployment.Name),
		RoleName:            aws.String(deployedCluster.RoleName()),
	}

	if _, err := iamSvc.AddRoleToInstanceProfile(roleInstanceProfileParams); err != nil {
		log.Errorf("Unable to add role to instance profile: %s", err)
		return err
	}

	return nil
}

/*
func setupAutoScaling(deployment *apis.Deployment, sess *session.Session, deployedCluster *DeployedCluster) error {
	svc := autoscaling.New(sess)

	userData := base64.StdEncoding.EncodeToString([]byte(
		`#!/bin/bash
echo ECS_CLUSTER=` + deployment.Name + " >> /etc/ecs/ecs.config"))

	params := &autoscaling.CreateLaunchConfigurationInput{
		LaunchConfigurationName:  aws.String(deployment.Name), // Required
		AssociatePublicIpAddress: aws.Bool(true),
		EbsOptimized:             aws.Bool(true),
		IamInstanceProfile:       aws.String(deployment.Name),
		ImageId:                  aws.String(ecsAmis[deployment.Region]),
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
		VPCZoneIdentifier:       aws.String(deployedCluster.SubnetId),
	}

	if _, err = svc.CreateAutoScalingGroup(autoScalingGroupParams); err != nil {
		return err
	}
	return nil
}
*/

func errorMessageFromFailures(failures []*ecs.Failure) string {
	var failureMessage = ""
	for _, failure := range failures {
		failureMessage += *failure.Reason + ", "
	}

	return failureMessage
}

// Max compare two inputs and return bigger one
func Max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func startService(deployedCluster *DeployedCluster, mapping *apis.NodeMapping, ecsSvc *ecs.ECS) error {
	log := deployedCluster.Logger
	serviceInput := &ecs.CreateServiceInput{
		DesiredCount:   aws.Int64(int64(1)),
		ServiceName:    aws.String(mapping.Service()),
		TaskDefinition: aws.String(mapping.Task),
		Cluster:        aws.String(deployedCluster.Deployment.Name),
		PlacementConstraints: []*ecs.PlacementConstraint{
			{
				Expression: aws.String(fmt.Sprintf("attribute:imageId == %s", mapping.ImageIdAttribute())),
				Type:       aws.String("memberOf"),
			},
		},
	}

	log.Infof("Starting service %v\n", serviceInput)
	if _, err := ecsSvc.CreateService(serviceInput); err != nil {
		return fmt.Errorf("Unable to start service %v\nError: %v\n", mapping.Service(), err)
	}

	return nil
}

func createServices(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	for _, mapping := range deployedCluster.Deployment.NodeMapping {
		startService(deployedCluster, &mapping, ecsSvc)
	}

	return nil
}

func waitUntilECSClusterReady(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	// Wait for ECS instances to be ready
	clusterReady := false
	totalCount := int64(len(deployedCluster.Deployment.ClusterDefinition.Nodes))
	describeClustersInput := &ecs.DescribeClustersInput{
		Clusters: []*string{&deployedCluster.Deployment.Name},
	}

	for !clusterReady {
		if describeOutput, err := ecsSvc.DescribeClusters(describeClustersInput); err != nil {
			return errors.New("Unable to list ECS clusters: " + err.Error())
		} else if len(describeOutput.Failures) > 0 {
			return errors.New(
				"Unable to list ECS clusters: " + errorMessageFromFailures(describeOutput.Failures))
		} else {
			registeredCount := *describeOutput.Clusters[0].RegisteredContainerInstancesCount

			if registeredCount >= totalCount {
				break
			} else {
				log.Infof("Cluster not ready. registered %d, total %v", registeredCount, totalCount)
				time.Sleep(time.Duration(3) * time.Second)
			}
		}
	}

	return nil
}

func populatePublicDnsNames(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	// We need to describe instances again to obtain the PublicDnsAddresses for ssh.
	describeInstanceOutput, err := ec2Svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: deployedCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to describe instances: " + err.Error())
	}

	for _, reservation := range describeInstanceOutput.Reservations {
		for _, instance := range reservation.Instances {
			if instance.PublicDnsName != nil && *instance.PublicDnsName != "" {
				for nodeId, nodeInfo := range deployedCluster.NodeInfos {
					if *nodeInfo.Instance.InstanceId == *instance.InstanceId {
						log.Infof("Assigning public dns name %s to node %d",
							*instance.PublicDnsName, nodeId)
						nodeInfo.PublicDnsName = *instance.PublicDnsName
						nodeInfo.PrivateIp = *instance.PrivateIpAddress
					}
				}
			}
		}
	}

	return nil
}

func setupInstanceAttribute(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	var containerInstances []*string
	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(deployedCluster.Deployment.Name),
	}

	listInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput)

	if err != nil {
		return errors.New("Unable to list container instances: " + err.Error())
	}

	containerInstances = listInstancesOutput.ContainerInstanceArns

	describeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deployedCluster.Deployment.Name),
		ContainerInstances: containerInstances,
	}

	describeInstancesOutput, err := ecsSvc.DescribeContainerInstances(describeInstancesInput)

	if err != nil {
		return errors.New("Unable to describe container instances: " + err.Error())
	}

	for _, instance := range describeInstancesOutput.ContainerInstances {
		for _, nodeInfo := range deployedCluster.NodeInfos {
			if *instance.Ec2InstanceId == *nodeInfo.Instance.InstanceId {
				nodeInfo.Arn = *instance.ContainerInstanceArn
				break
			}
		}
	}

	for _, mapping := range deployedCluster.Deployment.NodeMapping {
		nodeInfo, ok := deployedCluster.NodeInfos[mapping.Id]
		if !ok {
			return fmt.Errorf("Unable to find Node id %d in instance map", mapping.Id)
		}

		params := &ecs.PutAttributesInput{
			Attributes: []*ecs.Attribute{
				{
					Name:       aws.String("imageId"),
					TargetId:   aws.String(nodeInfo.Arn),
					TargetType: aws.String("container-instance"),
					Value:      aws.String(mapping.ImageIdAttribute()),
				},
			},
			Cluster: aws.String(deployedCluster.Deployment.Name),
		}

		_, err := ecsSvc.PutAttributes(params)

		if err != nil {
			return fmt.Errorf("Unable to put attribute on ECS instance: %v\nMessage:%s\n", params, err.Error())
		}
	}
	return nil
}

func createAWSLogsGroup(groupName string, svc *cloudwatchlogs.CloudWatchLogs) error {
	params := &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(groupName),
	}
	_, err := svc.CreateLogGroup(params)

	if err != nil {
		if strings.Contains(err.Error(), "ResourceAlreadyExistsException") {
			glog.Infof("Skip creating log group %s as it has already been created", groupName)
			return nil
		}

		errMsg := fmt.Sprintf("Unable to create the AWS log group of %s.\nException Message: %s\n", groupName, err.Error())
		glog.Warning(errMsg)
		return errors.New(errMsg)
	}

	return nil
}

func setupAWSLogsGroup(sess *session.Session, deployment *apis.Deployment) error {
	svc := cloudwatchlogs.New(sess)
	for _, task := range deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			// NOTE assume the log group does not exist and ignore any error.
			// Error types:
			// * InvalidParameterException
			// A parameter is specified incorrectly.
			// 	* ResourceAlreadyExistsException
			// The specified resource already exists.
			// 	* LimitExceededException
			// You have reached the maximum number of resources that can be created.
			// 	* OperationAbortedException
			// Multiple requests to update the same resource were in conflict.
			// 	* ServiceUnavailableException
			// The service cannot complete the request.
			// https://docs.aws.amazon.com/sdk-for-go/api/service/cloudwatchlogs/#example_CloudWatchLogs_CreateLogGroup
			createAWSLogsGroup(*container.Name, svc)

			// NOTE ensure each container definition containing logConfiguration field.
			container.LogConfiguration = &ecs.LogConfiguration{
				LogDriver: aws.String("awslogs"),
				Options: map[string]*string{
					"awslogs-group":         container.Name,
					"awslogs-region":        aws.String(deployment.Region),
					"awslogs-stream-prefix": aws.String("awslogs"),
				},
			}
		}
	}
	return nil
}

func (ecsDeployer *ECSDeployer) SetupEC2Infra(user string, uploadedFiles map[string]string, ec2Svc *ec2.EC2, iamSvc *iam.IAM, images map[string]string) error {
	deployedCluster := ecsDeployer.DeploymentInfo.AwsInfo
	log := deployedCluster.Logger

	log.Infof("Setting up IAM Role")
	if err := setupIAM(iamSvc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup IAM: " + err.Error())
	}

	log.Infof("Setting up Network")
	if err := setupNetwork(ec2Svc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup Network: " + err.Error())
	}

	log.Infof("Launching EC2 instances")
	if err := setupEC2(ec2Svc, deployedCluster, images); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to setup EC2: " + err.Error())
	}

	log.Infof("Populating public dns names")
	if err := populatePublicDnsNames(ec2Svc, deployedCluster); err != nil {
		ecsDeployer.DeleteDeployment()
		return errors.New("Unable to populate public dns names: " + err.Error())
	}

	log.Infof("Uploading files to EC2 Instances")
	if err := uploadFiles(user, ec2Svc, uploadedFiles, deployedCluster); err != nil {
		return errors.New("Unable to upload files to EC2: " + err.Error())
	}

	return nil
}

func isRegionValid(region string, images map[string]string) bool {
	_, ok := images[region]
	return ok
}

func stopECSTasks(svc *ecs.ECS, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	errMsg := false
	params := &ecs.ListTasksInput{
		Cluster: aws.String(deployedCluster.Name),
	}
	resp, err := svc.ListTasks(params)
	if err != nil {
		log.Errorf("Unable to list tasks: %s", err.Error())
		return err
	}

	for _, task := range resp.TaskArns {
		stopTaskParams := &ecs.StopTaskInput{
			Cluster: aws.String(deployedCluster.Name),
			Task:    task,
			Reason:  aws.String("Gracefully stop by hyperpilot/deployer."),
		}

		// NOTE Should we store the output of svc.StopTask ?
		// https://docs.aws.amazon.com/sdk-for-go/api/service/ecs/#StopTaskOutput
		_, err := svc.StopTask(stopTaskParams)
		if err != nil {
			log.Errorf("Unable to stop task (%s) %s\n", *task, err.Error())
			errMsg = true
		}
	}

	if errMsg {
		return errors.New("Unable to clean up all the tasks.")
	}
	return nil
}

func updateECSService(svc *ecs.ECS, nodemapping *apis.NodeMapping, cluster string, count int) error {
	params := &ecs.UpdateServiceInput{
		Service:      aws.String(nodemapping.Service()),
		Cluster:      aws.String(cluster),
		DesiredCount: aws.Int64(int64(count)),
	}

	if _, err := svc.UpdateService(params); err != nil {
		return fmt.Errorf("Unable to update ECS service: %s\n", err.Error())
	}
	return nil
}

func deleteECSService(svc *ecs.ECS, nodemapping *apis.NodeMapping, cluster string) error {
	params := &ecs.DeleteServiceInput{
		Service: aws.String(nodemapping.Service()),
		Cluster: aws.String(cluster),
	}

	if _, err := svc.DeleteService(params); err != nil {
		return fmt.Errorf("Unable to delete ECS service (%s): %s\n", nodemapping.Service(), err.Error())
	}
	return nil
}

func stopECSServices(svc *ecs.ECS, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	errMsg := false
	for _, nodemapping := range deployedCluster.Deployment.NodeMapping {
		if err := updateECSService(svc, &nodemapping, deployedCluster.Name, 0); err != nil {
			errMsg = true
			log.Warningf("Unable to update ECS service service %s to 0:\nMessage:%v\n", nodemapping.Service(), err.Error())
		}

		if err := deleteECSService(svc, &nodemapping, deployedCluster.Name); err != nil {
			errMsg = true
			log.Warningf("Unable to delete ECS service (%s):\nMessage:%v\n", nodemapping.Service(), err.Error())
		}
	}

	if errMsg {
		return errors.New("Unable to clean up all the services.")
	}
	return nil
}

func deleteTaskDefinitions(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	var tasks []*string
	var errBool bool

	for _, taskDefinition := range deployedCluster.Deployment.TaskDefinitions {
		params := &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: taskDefinition.Family,
		}
		resp, err := ecsSvc.DescribeTaskDefinition(params)
		if err != nil {
			log.Warningf("Unable to describe task (%s) : %s\n", *taskDefinition.Family, err.Error())
			continue
		}

		tasks = append(tasks, aws.String(fmt.Sprintf("%s:%d", *taskDefinition.Family, *resp.TaskDefinition.Revision)))
	}

	for _, task := range tasks {
		if _, err := ecsSvc.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{TaskDefinition: task}); err != nil {
			log.Warningf("Unable to de-register task definition: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to clean up task definitions")
	}

	return nil
}

func deleteIAM(iamSvc *iam.IAM, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	// NOTE For now (2016/12/28), we don't know how to handle the error of request timeout from AWS SDK.
	// Ignore all the errors and log them as error messages into log file. deleteIAM has different severity,
	//in contrast to other functions. Since the error of deleteIAM causes next time deployment's failure .

	errBool := false

	// remove role from instance profile
	roleInstanceProfileParam := &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Name),
		RoleName:            aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.RemoveRoleFromInstanceProfile(roleInstanceProfileParam); err != nil {
		log.Errorf("Unable to delete role %s from instance profile: %s\n",
			deployedCluster.RoleName(), err.Error())
		errBool = true
	}

	// delete instance profile
	instanceProfile := &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Name),
	}
	if _, err := iamSvc.DeleteInstanceProfile(instanceProfile); err != nil {
		log.Errorf("Unable to delete instance profile: %s\n", err.Error())
		errBool = true
	}

	// delete role policy
	rolePolicyParams := &iam.DeleteRolePolicyInput{
		PolicyName: aws.String(deployedCluster.PolicyName()),
		RoleName:   aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRolePolicy(rolePolicyParams); err != nil {
		log.Errorf("Unable to delete role policy IAM role: %s\n", err.Error())
		errBool = true
	}

	// delete role
	roleParams := &iam.DeleteRoleInput{
		RoleName: aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRole(roleParams); err != nil {
		log.Errorf("Unable to delete IAM role: %s", err.Error())
		errBool = true
	}

	if errBool {
		return errors.New("Unable to clean up AWS IAM")
	}

	return nil
}

func DeleteKeyPair(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	params := &ec2.DeleteKeyPairInput{
		KeyName: aws.String(deployedCluster.KeyName()),
	}
	if _, err := ec2Svc.DeleteKeyPair(params); err != nil {
		return fmt.Errorf("Unable to delete key pair: %s", err.Error())
	}
	return nil
}

func deleteSecurityGroup(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	errBool := false
	describeParams := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("group-name"),
				Values: []*string{
					aws.String(deployedCluster.Name),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeSecurityGroups(describeParams)
	if err != nil {
		return fmt.Errorf("Unable to describe tags of security group: %s\n", err.Error())
	}

	for _, group := range resp.SecurityGroups {
		params := &ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		}
		if _, err := ec2Svc.DeleteSecurityGroup(params); err != nil {
			log.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func deleteSubnet(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	errBool := false
	params := &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("resource-type"),
				Values: []*string{
					aws.String("subnet"),
				},
			},
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String(deployedCluster.SubnetName()),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeTags(params)

	if err != nil {
		return fmt.Errorf("Unable to describe tags of subnet: %s\n", err.Error())
	}

	for _, subnet := range resp.Tags {
		params := &ec2.DeleteSubnetInput{
			SubnetId: subnet.ResourceId,
		}
		_, err := ec2Svc.DeleteSubnet(params)
		if err != nil {
			log.Warningf("Unable to delete subnet (%s) %s\n", *subnet.ResourceId, err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to clan up subnet")
	}
	return nil
}

func checkVPC(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	if deployedCluster.VpcId == "" {
		params := &ec2.DescribeTagsInput{
			Filters: []*ec2.Filter{
				{
					Name: aws.String("resource-type"),
					Values: []*string{
						aws.String("vpc"),
					},
				},
				{
					Name: aws.String("tag:Name"),
					Values: []*string{
						aws.String(deployedCluster.VPCName()),
					},
				},
			},
		}
		resp, err := ec2Svc.DescribeTags(params)
		if err != nil {
			return fmt.Errorf("Unable to describe tags for VPC: %s\n", err.Error())
		} else if len(resp.Tags) <= 0 {
			return errors.New("Can not find VPC")
		}
		deployedCluster.VpcId = *resp.Tags[0].ResourceId
	}
	return nil
}

func deleteInternetGateway(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	log := deployedCluster.Logger
	params := &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("resource-type"),
				Values: []*string{
					aws.String("internet-gateway"),
				},
			},
			{
				Name: aws.String("tag:Name"),
				Values: []*string{
					aws.String(deployedCluster.Name),
				},
			},
		},
	}

	resp, err := ec2Svc.DescribeTags(params)
	if err != nil {
		log.Warningf("Unable to describe tags: %s\n", err.Error())
	} else if len(resp.Tags) == 0 {
		log.Warningf("Unable to find the internet gateway (%s)\n", deployedCluster.Name)
	} else {
		detachGatewayParams := &ec2.DetachInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
			VpcId:             aws.String(deployedCluster.VpcId),
		}
		if _, err := ec2Svc.DetachInternetGateway(detachGatewayParams); err != nil {
			log.Warningf("Unable to detach InternetGateway: %s\n", err.Error())
		}

		deleteGatewayParams := &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
		}
		if _, err := ec2Svc.DeleteInternetGateway(deleteGatewayParams); err != nil {
			log.Warningf("Unable to delete internet gateway: %s\n", err.Error())
		}
	}

	return nil
}

func deleteVPC(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	params := &ec2.DeleteVpcInput{
		VpcId: aws.String(deployedCluster.VpcId),
	}

	if _, err := ec2Svc.DeleteVpc(params); err != nil {
		return errors.New("Unable to delete VPC: " + err.Error())
	}
	return nil
}

func deleteEC2(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	var instanceIds []*string

	for _, id := range deployedCluster.InstanceIds {
		instanceIds = append(instanceIds, id)
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

func deleteCluster(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	params := &ecs.DeleteClusterInput{
		Cluster: aws.String(deployedCluster.Name),
	}

	if _, err := ecsSvc.DeleteCluster(params); err != nil {
		return fmt.Errorf("Unable to delete cluster: %s", err.Error())
	}

	return nil
}

// ReloadClusterState reload EC2 cluster state
func ReloadClusterState(awsProfile *AWSProfile, deployedCluster *DeployedCluster) error {
	sess, sessionErr := CreateSession(awsProfile, deployedCluster.Deployment)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	deploymentName := deployedCluster.Deployment.Name
	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(deploymentName),
	}

	listContainerInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput)
	if err != nil {
		return fmt.Errorf("Unable to list container instances: %s", err.Error())
	}

	ecsDescribeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deploymentName),
		ContainerInstances: listContainerInstancesOutput.ContainerInstanceArns,
	}

	ecsDescribeInstancesOutput, err := ecsSvc.DescribeContainerInstances(ecsDescribeInstancesInput)
	if err != nil {
		return fmt.Errorf("Unable to describe container instances: %s", err.Error())
	}

	var instanceIds []*string
	for _, containerInstance := range ecsDescribeInstancesOutput.ContainerInstances {
		instanceIds = append(instanceIds, containerInstance.Ec2InstanceId)
	}
	deployedCluster.InstanceIds = instanceIds

	if err := checkVPC(ec2Svc, deployedCluster); err != nil {
		return fmt.Errorf("Unable to find VPC: %s", err.Error())
	}

	return nil
}

// ReloadKeyPair reload KeyPair by keyName
func ReloadKeyPair(awsProfile *AWSProfile, deployedCluster *DeployedCluster, keyMaterial string) error {
	sess, sessionErr := CreateSession(awsProfile, deployedCluster.Deployment)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	describeKeyPairsInput := &ec2.DescribeKeyPairsInput{
		KeyNames: []*string{
			aws.String(deployedCluster.KeyName()),
		},
	}

	describeKeyPairsOutput, err := ec2Svc.DescribeKeyPairs(describeKeyPairsInput)
	if err != nil {
		return fmt.Errorf("Unable to describe keyPairs: %s", err.Error())
	}

	if len(describeKeyPairsOutput.KeyPairs) == 0 {
		return fmt.Errorf("Unable to find %s keyPairs", deployedCluster.Deployment.Name)
	}

	keyPair := &ec2.CreateKeyPairOutput{
		KeyName:        describeKeyPairsOutput.KeyPairs[0].KeyName,
		KeyFingerprint: describeKeyPairsOutput.KeyPairs[0].KeyFingerprint,
		KeyMaterial:    aws.String(keyMaterial),
	}
	deployedCluster.KeyPair = keyPair

	return nil
}

func GetClusterInfo(awsProfile *AWSProfile, deployedCluster *DeployedCluster) (*ClusterInfo, error) {
	sess, sessionErr := CreateSession(awsProfile, deployedCluster.Deployment)
	if sessionErr != nil {
		return nil, fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	deploymentName := deployedCluster.Deployment.Name
	listInstancesInput := &ecs.ListContainerInstancesInput{
		Cluster: aws.String(deploymentName),
	}

	listContainerInstancesOutput, err := ecsSvc.ListContainerInstances(listInstancesInput)
	if err != nil {
		return nil, fmt.Errorf("Unable to list container instances: %s", err.Error())
	}

	ecsDescribeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deploymentName),
		ContainerInstances: listContainerInstancesOutput.ContainerInstanceArns,
	}

	ecsDescribeInstancesOutput, err := ecsSvc.DescribeContainerInstances(ecsDescribeInstancesInput)
	if err != nil {
		return nil, fmt.Errorf("Unable to describe container instances: %s", err.Error())
	}

	var instanceIds []*string
	for _, containerInstance := range ecsDescribeInstancesOutput.ContainerInstances {
		instanceIds = append(instanceIds, containerInstance.Ec2InstanceId)
	}

	ec2DescribeInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	ec2DescribeInstanceOutput, err := ec2Svc.DescribeInstances(ec2DescribeInstancesInput)
	if err != nil {
		return nil, fmt.Errorf("Unable to describe ec2 instances: %s", err.Error())
	}

	listTasksInput := &ecs.ListTasksInput{
		Cluster: aws.String(deploymentName),
	}

	listTasksOutput, err := ecsSvc.ListTasks(listTasksInput)
	if err != nil {
		return nil, fmt.Errorf("Unable to list ecs tasks: %s", err.Error())
	}

	describeTasksInput := &ecs.DescribeTasksInput{
		Tasks:   listTasksOutput.TaskArns,
		Cluster: aws.String(deploymentName),
	}

	describeTasksOutput, err := ecsSvc.DescribeTasks(describeTasksInput)
	if err != nil {
		return nil, fmt.Errorf("Unable to describe ecs tasks: %s", err.Error())
	}

	clusterInfo := &ClusterInfo{
		ContainerInstances: ecsDescribeInstancesOutput.ContainerInstances,
		Reservations:       ec2DescribeInstanceOutput.Reservations,
		Tasks:              describeTasksOutput.Tasks,
	}

	return clusterInfo, nil
}

func GetServiceUrl(deployedCluster *DeployedCluster, serviceName string) (string, error) {
	nodePort := ""
	taskFamilyName := ""
	for _, task := range deployedCluster.Deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			if *container.Name == serviceName {
				nodePort = strconv.FormatInt(*container.PortMappings[0].HostPort, 10)
				taskFamilyName = *task.Family
				break
			}
		}
	}

	if nodePort == "" {
		return "", errors.New("Unable to find container in deployment container defintiions")
	}

	nodeId := -1
	for _, nodeMapping := range deployedCluster.Deployment.NodeMapping {
		if nodeMapping.Task == taskFamilyName {
			nodeId = nodeMapping.Id
			break
		}
	}

	if nodeId == -1 {
		return "", errors.New("Unable to find task in deployment node mappings")
	}

	nodeInfo, nodeOk := deployedCluster.NodeInfos[nodeId]
	if !nodeOk {
		return "", errors.New("Unable to find node in cluster")
	}

	return nodeInfo.PublicDnsName + ":" + nodePort, nil
}
