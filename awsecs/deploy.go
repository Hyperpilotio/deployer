package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang/glog"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
)

// DeployedCluster stores the data of a cluster
type DeployedCluster struct {
	Name        *string
	KeyPair     *ec2.CreateKeyPairOutput
	Deployment  *Deployment
	InstanceIds map[int]*string
}

func setupECS(deployment *Deployment, ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	// FIXME checke if the cluster exists or not
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String("weave-ecs-demo-cluster"),
	}

	if _, err := ecsSvc.CreateCluster(clusterParams); err != nil {
		return err
	}

	for _, taskDefinition := range deployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			glog.Errorln("Unable to register task definition", err)
			DeleteDeployment(deployedCluster)
			return err
		}
	}

	return nil
}

func setNetwork(ec2Svc *ec2.EC2, deployedCluster *DeployedCluster) error {

	// create vpc
	vpcParams := &ec2.CreateVpcInput{
		CidrBlock:                   aws.String("172.31.0.0/28"),
		AmazonProvidedIpv6CidrBlock: aws.Bool(true),
		DryRun:          aws.Bool(true),
		InstanceTenancy: aws.String("Tenancy"),
	}

	resp, err := ec2Svc.CreateVpc(vpcParams)

	if err != nil {
		return err
	}

	vpcAttributeParams := &ec2.ModifyVpcAttributeInput{
		VpcId: resp.Vpc.VpcId,
		EnableDnsHostnames: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
		EnableDnsSupport: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}

	if _, err = ec2Svc.ModifyVpcAttribute(vpcAttributeParams); err != nil {
		return err
	}

	tagParams := &ec2.CreateTagsInput{
		Resources: []*string{resp.Vpc.VpcId},
		DryRun:    aws.Bool(true),
		Tags: []*ec2.Tag{&ec2.Tag{
			Key:   aws.String("Name"),
			Value: aws.String("weave-ecs-demo-vpc"),
		}},
	}
	if _, err = ec2Svc.CreateTags(tagParams); err != nil {
		return err
	}

	// NOTE skip creating the subnet
	// NOTE skip aws internet gateway

	// create security group

	securityGroupParams := &ec2.CreateSecurityGroupInput{
		Description: aws.String("Weave Ecs Demo"), // Required
		GroupName:   aws.String("weave-ecs-demo"), // Required
		DryRun:      aws.Bool(true),
		VpcId:       resp.Vpc.VpcId,
	}
	securityGroupResp, err := ec2Svc.CreateSecurityGroup(securityGroupParams)
	if err != nil {
		return err
	}

	// port 22, 80, 4040
	portArr := []int{22, 80, 4040}

	for _, port := range portArr {
		securityGroupIngressParams := &ec2.AuthorizeSecurityGroupIngressInput{
			CidrIp:     aws.String("0.0.0.0/0"),
			DryRun:     aws.Bool(true),
			FromPort:   aws.Int64(int64(port)),
			GroupId:    securityGroupResp.GroupId,
			GroupName:  aws.String("weave-ecs-demo"),
			IpProtocol: aws.String("tcp"),
		}
		_, err = ec2Svc.AuthorizeSecurityGroupIngress(securityGroupIngressParams)
		if err != nil {
			return err
		}
	}
	// port 6783 tcp, 6783 udp, 6784 udp, 4040 tcp
	portProtocolArr := []string{"6783-tcp", "6783-udp", "6784-udp", "4040-tcp"}

	for _, item := range portProtocolArr {
		i, _ := strconv.Atoi(strings.Split(item, "-")[0])
		securityGroupIngressParams := &ec2.AuthorizeSecurityGroupIngressInput{
			DryRun:                     aws.Bool(true),
			FromPort:                   aws.Int64(int64(i)),
			GroupId:                    securityGroupResp.GroupId,
			GroupName:                  aws.String("weave-ecs-demo"),
			IpProtocol:                 aws.String(strings.Split(item, "-")[1]),
			SourceSecurityGroupName:    aws.String("weave-ecs-demo"),
			SourceSecurityGroupOwnerId: securityGroupResp.GroupId,
		}
		_, err = ec2Svc.AuthorizeSecurityGroupIngress(securityGroupIngressParams)
		if err != nil {
			return err
		}
	}
	return nil
}

func setupEC2(deployment *Deployment, sess *session.Session, deployedCluster *DeployedCluster) error {
	iamSvc := iam.New(sess)

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(deployment.Name),
	}
	_, err := iamSvc.CreateInstanceProfile(iamParams)
	if err != nil {
		glog.Errorf("Failed to create instance profile: %s", err)
		DeleteDeployment(deployedCluster)
		return err
	}

	ec2Svc := ec2.New(sess)

	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployment.Name),
	}
	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		glog.Errorf("Failed to create key pair: %s", keyErr)
		DeleteDeployment(deployedCluster)
		return keyErr
	}

	deployedCluster.KeyPair = keyOutput
	userData := base64.StdEncoding.EncodeToString([]byte(
		`#!/bin/bash
echo ECS_CLUSTER=` + deployment.Name + " >> /etc/ecs/ecs.config"))

	for _, node := range deployment.ClusterDefinition.Nodes {
		runResult, runErr := ec2Svc.RunInstances(&ec2.RunInstancesInput{
			KeyName:      aws.String(*keyOutput.KeyName),
			ImageId:      aws.String(node.ImageId),
			InstanceType: aws.String(node.InstanceType),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
			UserData:     aws.String(userData),
		})
		if runErr != nil {
			glog.Errorln("Unable to run ec2 instance %s: %s", node.Id, runErr)
			DeleteDeployment(deployedCluster)
			return runErr
		}
		deployedCluster.InstanceIds[node.Id] = runResult.Instances[0].InstanceId
		_, tagErr := ec2Svc.CreateTags(&ec2.CreateTagsInput{
			Resources: []*string{runResult.Instances[0].InstanceId},
			Tags: []*ec2.Tag{
				{
					Key:   aws.String("Deployment"),
					Value: aws.String(deployment.Name),
				},
			},
		})
		if tagErr != nil {
			glog.Errorln("Could not create tags for instance", runResult.Instances[0].InstanceId, tagErr)
			DeleteDeployment(deployedCluster)
			return tagErr
		}
	}

	ids := make([]*string, 0, len(deployedCluster.InstanceIds))

	for _, value := range deployedCluster.InstanceIds {
		ids = append(ids, value)
	}
	ec2Svc.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{
		InstanceIds: ids,
	})

	return nil
}

func launchECSTasks(deployment *Deployment, ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	for _, mapping := range deployment.NodeMapping {
		instanceID, ok := deployedCluster.InstanceIds[mapping.Id]
		if !ok {
			err := fmt.Sprintf("Unable to find Node id %d in instance map", mapping.Id)
			glog.Error(err)
			DeleteDeployment(deployedCluster)
			return errors.New(err)
		}

		startTaskOutput, err := ecsSvc.StartTask(&ecs.StartTaskInput{
			Cluster:            aws.String(deployment.Name),
			TaskDefinition:     aws.String(mapping.Task),
			ContainerInstances: []*string{instanceID},
		})

		if err != nil {
			glog.Errorf("Unable to start task %v\nError: %v", mapping.Task, err)
			DeleteDeployment(deployedCluster)
			return err
		}

		if len(startTaskOutput.Failures) > 0 {
			var failureMessage = ""
			for _, failure := range startTaskOutput.Failures {
				failureMessage += *failure.Reason + ", "
			}
			errorMessage := fmt.Sprintf("Failed to start task %v\nMessage: %v", mapping.Task, failureMessage)
			glog.Errorf(errorMessage)
			DeleteDeployment(deployedCluster)
			return errors.New(errorMessage)
		}
	}

	return nil
}

// CreateDeployment start a deployment
func CreateDeployment(viper *viper.Viper, deployment *Deployment) (*DeployedCluster, error) {
	awsID := viper.GetString("awsId")
	awsSecret := viper.GetString("awsSecret")
	creds := credentials.NewStaticCredentials(awsID, awsSecret, "")
	config := &aws.Config{
		Region: aws.String(deployment.Region),
	}
	config = config.WithCredentials(creds)
	sess, err := session.NewSession(config)
	if err != nil {
		glog.Errorf("Failed to create session: %s", err)
		return nil, err
	}
	deployedCluster := &DeployedCluster{
		Name:       &deployment.Name,
		Deployment: deployment,
	}
	ecsSvc := ecs.New(sess)
	if err = setupECS(deployment, ecsSvc, deployedCluster); err != nil {
		return nil, err
	}

	ec2Svc := ec2.New(sess)
	setNetwork(ec2Svc, deployedCluster)

	if err = setupEC2(deployment, sess, deployedCluster); err != nil {
		return nil, err
	}
	if err = launchECSTasks(deployment, ecsSvc, deployedCluster); err != nil {
		return nil, err
	}

	return deployedCluster, nil
}

// DeleteDeployment clean up the cluster from AWS ECS.
func DeleteDeployment(deployedCluster *DeployedCluster) error {

	return nil
}
