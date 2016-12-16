package awsecs

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/golang/glog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
)

type DeployedCluster struct {
	Name        *string
	KeyPair     *ec2.CreateKeyPairOutput
	Deployment  *Deployment
	InstanceIds map[int]*string
}

func setupECS(deployment *Deployment, ecsSvc *ecs.ECS, deployedCluster *DeployedCluster) error {
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String("String"),
	}

	if _, err := ecsSvc.CreateCluster(clusterParams); err != nil {
		return err
	}

	for _, taskDefinition := range deployment.TaskDefinitions {
		if _, err := ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			glog.Errorf("Unable to register task definition", err)
			DeleteDeployment(deployment)
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
		DeleteDeployment(deployment)
		return err
	}

	ec2Svc := ec2.New(sess)

	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployment.Name),
	}
	keyOutput, keyErr := ec2Svc.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		glog.Errorf("Failed to create key pair: %s", keyErr)
		DeleteDeployment(deployment)
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
			glog.Errorf("Unable to run ec2 instance %s: %s", node.Id, runErr)
			DeleteDeployment(deployment)
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
			glog.Errorf("Could not create tags for instance", runResult.Instances[0].InstanceId, tagErr)
			DeleteDeployment(deployment)
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
		instanceId, ok := deployedCluster.InstanceIds[mapping.Id]
		if !ok {
			err := fmt.Sprintf("Unable to find Node id %s in instance map", mapping.Id)
			glog.Error(err)
			DeleteDeployment(deployment)
			return errors.New(err)
		}

		startTaskOutput, err := ecsSvc.StartTask(&ecs.StartTaskInput{
			Cluster:            aws.String(deployment.Name),
			TaskDefinition:     aws.String(mapping.Task),
			ContainerInstances: []*string{instanceId},
		})

		if err != nil {
			glog.Errorf("Unable to start task", mapping.Task, err)
			DeleteDeployment(deployment)
			return err
		}

		if len(startTaskOutput.Failures) > 0 {
			var failureMessage = ""
			for _, failure := range startTaskOutput.Failures {
				failureMessage += *failure.Reason + ", "
			}
			errorMessage := fmt.Sprintf("Failed to start task", mapping.Task, failureMessage)
			glog.Errorf(errorMessage)
			DeleteDeployment(deployment)
			return errors.New(errorMessage)
		}
	}

	return nil
}

func CreateDeployment(deployment *Deployment) (*DeployedCluster, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String(deployment.Region)})
	if err != nil {
		glog.Errorf("Failed to create session: %s", err)
		return nil, err
	}
	deployedCluster := &DeployedCluster{
		Name: &deployment.Name,
	}
	ecsSvc := ecs.New(sess)
	if err = setupECS(deployment, ecsSvc, deployedCluster); err != nil {
		return nil, err
	}
	if err = setupEC2(deployment, sess, deployedCluster); err != nil {
		return nil, err
	}
	if err = launchECSTasks(deployment, ecsSvc, deployedCluster); err != nil {
		return nil, err
	}

	return deployedCluster, nil
}

func DeleteDeployment(deployment *Deployment) error {
	return nil
}
