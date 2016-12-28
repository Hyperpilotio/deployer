package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/golang/glog"

	"github.com/hyperpilotio/deployer/common"

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
	Instances         map[int]*ec2.Instance
	InstanceIds       []*string
	VpcId             string
}

// KeyName return a key name according to the Deployment.Name with suffix "-key"
func (deployedCluster *DeployedCluster) KeyName() string {
	return deployedCluster.Deployment.Name + "-key"
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
		return errors.New("Unable to create cluster: " + err.Error())
	}

	for _, taskDefinition := range deployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
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

	glog.V(1).Infoln("Tagging VPC with deployment name")
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
	if err := createTag(ec2Svc, []*string{&deployedCluster.SubnetId}, "Name", deployedCluster.SubnetName()); err != nil {
		return errors.New("Unable to tag subnet: " + err.Error())
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
		Description: aws.String(deployment.Name),
		GroupName:   aws.String(deployment.Name),
		VpcId:       aws.String(deployedCluster.VpcId),
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

func uploadFiles(deployment *Deployment, ec2Svc *ec2.EC2, uploadedFiles map[string]string, deployedCluster *DeployedCluster) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	// We need to describe instances again to obtain the PublicDnsAddresses for ssh.
	describeInstanceOutput, err := ec2Svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: deployedCluster.InstanceIds,
	})
	if err != nil {
		return errors.New("Unable to describe instances: " + err.Error())
	}

	publicAddresses := make([]*string, 0)
	for _, reservation := range describeInstanceOutput.Reservations {
		for _, instance := range reservation.Instances {
			if instance.PublicDnsName != nil && *instance.PublicDnsName != "" {
				publicAddresses = append(publicAddresses, instance.PublicDnsName)
			}
		}
	}

	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := ssh.ClientConfig{
		User: "ec2-user",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		}}

	for _, publicAddress := range publicAddresses {
		address := *publicAddress + ":22"
		scpClient := common.NewScpClient(address, &clientConfig)
		if err := scpClient.Connect(); err != nil {
			return errors.New("Unable to connect to server " + address + ": " + err.Error())
		}

		for _, deployFile := range deployment.Files {
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

			if err := scpClient.CopyFile(f, deployFile.Path, "0644"); err != nil {
				errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, address, err.Error())
				return errors.New(errorMsg)
			}
		}
	}

	return nil
}

func setupEC2(deployment *Deployment, ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployedCluster.KeyName()),
	}
	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
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
			return errors.New("Unable to run ec2 instance '" + strconv.Itoa(node.Id) + "': " + runErr.Error())
		}

		deployedCluster.Instances[node.Id] = runResult.Instances[0]
		deployedCluster.InstanceIds = append(deployedCluster.InstanceIds, runResult.Instances[0].InstanceId)
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

	nodeCount := len(deployedCluster.InstanceIds)
	glog.V(1).Infof("Waitng for %d EC2 instances to exist", nodeCount)

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

	glog.V(1).Infof("Waitng for %d EC2 instances to be status ok", nodeCount)
	if err := ec2Svc.WaitUntilInstanceStatusOk(describeInstanceStatusInput); err != nil {
		return errors.New("Unable to wait for ec2 instances be status ok: " + err.Error())
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
		glog.Errorf("Unable to create IAM role: %s", err)
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
		glog.Errorf("Unable to put role policy: %s", err)
		return err
	}

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(deployment.Name),
	}

	if _, err := iamSvc.CreateInstanceProfile(iamParams); err != nil {
		glog.Errorf("Unable to create instance profile: %s", err)
		return err
	}

	roleInstanceProfileParams := &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(deployment.Name),
		RoleName:            aws.String(deployedCluster.RoleName()),
	}

	if _, err := iamSvc.AddRoleToInstanceProfile(roleInstanceProfileParams); err != nil {
		glog.Errorf("Unable to add role to instance profile: %s", err)
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

func Max(x, y int) int {
	if x > y {
		return x
	}
	return y
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
				"Unable to list ECS clusters: " + errorMessageFromFailures(describeOutput.Failures))
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
		return errors.New("Unable to list container instances: " + err.Error())
	} else {
		containerInstances = listInstancesOutput.ContainerInstanceArns
	}

	describeInstancesInput := &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(deployment.Name),
		ContainerInstances: containerInstances,
	}

	if describeInstancesOutput, err := ecsSvc.DescribeContainerInstances(describeInstancesInput); err != nil {
		return errors.New("Unable to describe container instances: " + err.Error())
	} else {
		for _, instance := range describeInstancesOutput.ContainerInstances {
			for key, value := range deployedCluster.Instances {
				if *instance.Ec2InstanceId == *value.InstanceId {
					nodeIdToArn[key] = *instance.ContainerInstanceArn
					break
				}
			}
		}
	}

	glog.V(1).Infof("Node Id to Arn: %v", nodeIdToArn)
	for _, mapping := range deployment.NodeMapping {
		instanceId, ok := nodeIdToArn[mapping.Id]
		if !ok {
			err := fmt.Sprintf("Unable to find Node id %d in instance map", mapping.Id)
			glog.Error(err)
			return errors.New(err)
		}

		startTaskInput := &ecs.StartTaskInput{
			Cluster:            aws.String(deployment.Name),
			TaskDefinition:     aws.String(mapping.Task),
			ContainerInstances: []*string{&instanceId},
		}

		glog.Infof("Starting task %s on node %d with count %d", mapping.Task, mapping.Id, mapping.Count)
		var count int = mapping.Count
		if count <= 0 {
			count = 1
		}
		for i := count; i <= Max(mapping.Count, 1); i++ {
			startTaskOutput, err := ecsSvc.StartTask(startTaskInput)
			if err != nil {
				startTaskErr := fmt.Sprintf("Unable to start task %v\nError: %v", mapping.Task, err)
				return errors.New(startTaskErr)
			}

			if len(startTaskOutput.Failures) > 0 {
				errorMessage := fmt.Sprintf(
					"Unable to start task %v\nMessage: %v",
					mapping.Task,
					errorMessageFromFailures(startTaskOutput.Failures))
				glog.Errorf(errorMessage)
				return errors.New(errorMessage)
			}
		}
	}

	return nil
}

// NewDeployedCluster return the instance of DeployedCluster
func NewDeployedCluster(deployment *Deployment) *DeployedCluster {
	return &DeployedCluster{
		Name:        deployment.Name,
		Deployment:  deployment,
		Instances:   make(map[int]*ec2.Instance),
		InstanceIds: make([]*string, 0),
	}
}

// CreateDeployment start a deployment
func CreateDeployment(viper *viper.Viper, deployment *Deployment, uploadedFiles map[string]string, deployedCluster *DeployedCluster) error {
	awsId := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	if !validateRegion(deployment) {
		return errors.New("Region is invalidate.")
	}
	config := &aws.Config{
		Region: aws.String(deployment.Region),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Unable to create session: %s", err)
		return err
	}
	ecsSvc := ecs.New(sess)
	ec2Svc := ec2.New(sess)
	if err = setupECS(deployment, ecsSvc, deployedCluster); err != nil {
		DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to setup ECS: " + err.Error())
	}

	if err = setupIAM(deployment, sess, deployedCluster); err != nil {
		DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to setup IAM: " + err.Error())
	}

	//if err = setupAutoScaling(deployment, sess, deployedCluster); err != nil {
	// DeleteDeployment(viper, deployedCluster)
	//	return nil, err
	//}

	if err = setupNetwork(deployment, ec2Svc, deployedCluster); err != nil {
		DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to setup Network: " + err.Error())
	}

	if err = setupEC2(deployment, ec2Svc, deployedCluster); err != nil {
		DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to setup EC2: " + err.Error())
	}

	if err = uploadFiles(deployment, ec2Svc, uploadedFiles, deployedCluster); err != nil {
		return errors.New("Unable to upload files to EC2: " + err.Error())
	}

	if err = launchECSTasks(deployment, ecsSvc, deployedCluster); err != nil {
		DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to launch ECS tasks: " + err.Error())
	}

	return nil
}

func validateRegion(d *Deployment) bool {
	if _, ok := amiCollection[d.Region]; ok {
		return true
	}

	keys := []string{}
	for key := range amiCollection {
		keys = append(keys, key)
	}
	glog.Warningf("The region of deployment is using %s, which doesn't offer ECS yet, please set it to one from: %v", d.Region, keys)
	return false
}

func stopECSTasks(svc *ecs.ECS, deployedCluster *DeployedCluster) error {
	errMsg := false
	params := &ecs.ListTasksInput{
		Cluster: aws.String(deployedCluster.Name),
	}
	resp, err := svc.ListTasks(params)
	if err != nil {
		glog.Errorln("Unable to list tasks: ", err.Error())
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
			glog.Errorf("Unable to stop task (%s) %s\n", *task, err.Error())
			errMsg = true
		}
	}

	if errMsg {
		return errors.New("Unable to clean up all the tasks.")
	}
	return nil
}

func deleteTaskDefinitions(ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	var tasks []*string
	var errBool bool

	for _, taskDefinition := range deployedCluster.Deployment.TaskDefinitions {
		params := &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: taskDefinition.Family,
		}
		resp, err := ecsSvc.DescribeTaskDefinition(params)
		if err != nil {
			glog.Warningf("Unable to describe task (%s) : %s\n", *taskDefinition.Family, err.Error())
			continue
		}

		tasks = append(tasks, aws.String(fmt.Sprintf("%s:%d", *taskDefinition.Family, *resp.TaskDefinition.Revision)))
	}

	for _, task := range tasks {
		if _, err := ecsSvc.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{TaskDefinition: task}); err != nil {
			glog.Warningf("Unable to de-register task definition: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to clean up task definitions")
	}

	return nil
}

func deleteIAM(sess *session.Session, deployedCluster *DeployedCluster) error {
	// NOTE For now (2016/12/28), we don't know how to handle the error of request timeout from AWS SDK.
	// Ignore all the errors and log them as error messages into log file. deleteIAM has different severity,
	//in contrast to other functions. Since the error of deleteIAM causes next time deployment's failure .

	errBool := false
	iamSvc := iam.New(sess)

	// remove role from instance profile
	roleInstanceProfileParam := &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Name),
		RoleName:            aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.RemoveRoleFromInstanceProfile(roleInstanceProfileParam); err != nil {
		glog.Errorf("Unable to delete role %s from instance profile: %s\n",
			deployedCluster.RoleName(), err.Error())
		errBool = true
	}

	// delete instance profile
	instanceProfile := &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(deployedCluster.Name),
	}
	if _, err := iamSvc.DeleteInstanceProfile(instanceProfile); err != nil {
		glog.Errorf("Unable to delete instance profile: %s\n", err.Error())
		errBool = true
	}

	// delete role policy
	rolePolicyParams := &iam.DeleteRolePolicyInput{
		PolicyName: aws.String(deployedCluster.PolicyName()),
		RoleName:   aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRolePolicy(rolePolicyParams); err != nil {
		glog.Errorf("Unable to delete role policy IAM role: %s\n", err.Error())
		errBool = true
	}

	// delete role
	roleParams := &iam.DeleteRoleInput{
		RoleName: aws.String(deployedCluster.RoleName()),
	}
	if _, err := iamSvc.DeleteRole(roleParams); err != nil {
		glog.Errorf("Unable to create IAM role: %s", err.Error())
		errBool = true
	}

	if errBool {
		return errors.New("Unable to clean up AWS IAM")
	}

	return nil
}

func deleteKeyPair(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
	params := &ec2.DeleteKeyPairInput{
		KeyName: aws.String(deployedCluster.KeyName()),
	}
	if _, err := ec2Svc.DeleteKeyPair(params); err != nil {
		return fmt.Errorf("Unable to delete key pair: %s", err.Error())
	}
	return nil
}

func deleteSecurityGroup(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
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
			glog.Warningf("Unable to delete security group: %s\n", err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New("Unable to delete all the relative security groups")
	}

	return nil
}

func deleteSubnet(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {
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
			glog.Warningf("Unable to delete subnet (%s) %s\n", *subnet.ResourceId, err.Error())
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
		glog.Warningf("Unable to describe tags: %s\n", err.Error())
	} else if len(resp.Tags) == 0 {
		glog.Warningf("Unable to find the internet gateway (%s)\n", deployedCluster.Name)
	} else {
		detachGatewayParams := &ec2.DetachInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
			VpcId:             aws.String(deployedCluster.VpcId),
		}
		if _, err := ec2Svc.DetachInternetGateway(detachGatewayParams); err != nil {
			glog.Warningf("Unable to detach InternetGateway: %s\n", err.Error())
		}

		deleteGatewayParams := &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: resp.Tags[0].ResourceId,
		}
		if _, err := ec2Svc.DeleteInternetGateway(deleteGatewayParams); err != nil {
			glog.Warningf("Unable to delete internet gateway: %s\n", err.Error())
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

// DeleteDeployment clean up the cluster from AWS ECS.
func DeleteDeployment(viper *viper.Viper, deployedCluster *DeployedCluster) error {
	// errBool is a flag to show if this deleteDeployment successed or not. If this action failed,
	// return error.
	errBool := false
	awsId := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsId, awsSecret, "")
	if !validateRegion(deployedCluster.Deployment) {
		return errors.New("Region is invalidate.")
	}

	config := &aws.Config{
		Region: aws.String(deployedCluster.Deployment.Region),
	}

	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorln("Unable to create session: ", err.Error())
	}

	ec2Svc := ec2.New(sess)
	ecsSvc := ecs.New(sess)

	err = checkVPC(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to find VPC: ", err.Error())
		errBool = true
	}

	// Stop all running tasks
	err = stopECSTasks(ecsSvc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to stop ECS tasks: ", err.Error())
		errBool = true
	}

	// delete all the task definitions
	err = deleteTaskDefinitions(ecsSvc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete task definitions: %s", err.Error())
		errBool = true
	}

	// Terminate EC2 instance
	err = deleteEC2(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete task definitions: %s", err.Error())
		errBool = true
	}

	// NOTE if we create autoscaling, delete it. Wait until the deletes all the instance.
	// Delete the launch configuration

	// delete IAM role
	err = deleteIAM(sess, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete IAM: %s", err.Error())
		errBool = true
	}

	// delete key pair
	err = deleteKeyPair(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete key pair: ", err.Error())
		errBool = true
	}

	// delete security group
	err = deleteSecurityGroup(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete security group: ", err.Error())
		errBool = true
	}

	// delete internet gateway.
	err = deleteInternetGateway(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete internet gateway: ", err.Error())
		errBool = true
	}

	// delete subnet.
	err = deleteSubnet(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete subnet: ", err.Error())
		errBool = true
	}

	// Delete VPC
	err = deleteVPC(ec2Svc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete VPC: ", err)
		errBool = true
	}

	// Delete ecs cluster
	err = deleteCluster(ecsSvc, deployedCluster)
	if err != nil {
		glog.Errorln("Unable to delete ECS cluster: ", err)
		errBool = true
	}

	if errBool {
		return errors.New("Unable to clean up whole deployment.")
	}
	return nil
}
