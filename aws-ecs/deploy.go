package ecs

import (
	"encoding/base64"

	"github.com/golang/glog"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
)

type DeployedCluster struct {
	name       *string
	KeyPair    *CreateKeyPairOutput
	deployment *Deployment
}

func setupECS(deployment *Deployment, ecsSvc *ECS, deployedCluster *DeployedCluster) error {
	clusterParams := &ecs.CreateClusterInput{
		ClusterName: aws.String("String"),
	}
	_, err = ecsSvc.CreateCluster(clusterParams)
	if _, err = ecs.CreateCluster(&input); err != nil {
		return err
	}

	for _, taskDefinition := range deployment.TaskDefinitions {
		if _, err = ecsSvc.RegisterTaskDefinition(&taskDefinition); err != nil {
			glog.Errorf("Unable to register task definition", err)
			DeleteDeployment(deployment)
			return err
		}
	}

	return nil
}

func setupEC2(deployment *Deployment, session *Session, deployedCluster *DeployedCluster) error {
	iamSvc := iam.New(sess)

	iamParams := &iam.CreateInstanceProfileInput{
		InstanceProfileName: deployment.Name,
	}
	_, err = iamSvc.CreateInstanceProfile(iamParams)
	if err != nil {
		glog.Errorf("Failed to create instance profile: %s", err)
		DeleteDeployment(deployment)
		return err
	}

	ec2Svc := ec2.new(sess)

	keyPairParams := &ec2.CreateKeyPairInput{
		KeyName: aws.String(deployment.Name),
	}
	keyOutput, keyErr := ec2.CreateKeyPair(keyPairParams)
	if keyErr != nil {
		glog.Errorf("Failed to create key pair: %s", keyErr)
		DeleteDeployment(deployment)
		return keyErr
	}

	userData := base64.StdEncoding.EncodeToString([]byte(
		`#!/bin/bash
echo ECS_CLUSTER=` + deployment.Name + " >> /etc/ecs/ecs.config"))

	var instanceIds map[int]string
	for _, node = range deployment.ClusterDefinition.Nodes {
		runResult, runErr := ec2Svc.RunInstances(&ec2.RunInstancesInput{
			KeyName:      aws.String(keyOutput.Name),
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
		instanceIds[node.Id] = runResult.Instances[0].InstanceId
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
			glog.Errorf("Could not create tags for instance", runResult.Instances[0].InstanceId, errtag)
			DeleteDeployment(deployment)
			return
		}
	}

	ec2Svc.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	})

	return nil
}

func launchECSTasks(deployment *Deployment, instanceIds map[int]*string, ecsSvc *ECS, deployedCluster *DeployedCluster) error {
	for _, mapping := range deployment.NodeMapping {
		instanceId, ok := instanceIds[mapping.Id]
		if !ok {
			err := fmt.Sprintf("Unable to find Node id %s in instance map", mapping.Id)
			glog.Error(err)
			DeleteDeployment(deployment)
			return error(err)
		}

		startTaskOutput, err := ecsSvc.StartTask(&ecs.StartTaskInput{
			Cluster:        aws.String(deployment.Name),
			Count:          aws.Int64(1),
			TaskDefinition: aws.String(mapping.Task),
		})

		if err != nil {
			glog.Errorf("Unable to start task", mapping.Task, err)
			DelteDeployment(deployment)
			return err
		}

		if count(startTaskOutput.Failures) > 0 {
			var failureMessage = ""
			for _, failure := range startTaskOutput.Failures {
				failureMessage += failure.Reason + ", "
			}
			errorMessage := fmt.Sprintf("Failed to start task", mapping.Task, failureMessage)
			glog.Errorf(errorMessage)
			DeleteDeployment(deployment)
			return error(errorMessage)
		}
	}

	return nil
}

func CreateDeployment(deployment *Deployment) (DeployedCluster, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String(deployment.Region)})
	if err != nil {
		glog.Errorf("Failed to create session: %s", err)
		return err
	}
	deployedCluster := &DeployCluster{}
	ecsSvc := ecs.New(sess)
	if err = setupECS(deployment, ecsSvc, deployedCluster); err != nil {
		return err
	}
	if err = setupEC2(deployment, sess, deployedCluster); err != nil {
		return err
	}
	if err = launchECSTasks(deployment, ecsSvc, deployedCluster); err != nil {
		return err
	}
}

func DeleteDeployment(deployment *Deployment) error {

}
