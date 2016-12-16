package awsecs

import (
	"github.com/aws/aws-sdk-go/service/ecs"
)

type ClusterDefinition struct {
	Nodes []struct {
		Id           int    `form:"id" json:"id" binding:"required"`
		InstanceType string `form:"instanceType" json:"instanceType" binding:"required"`
		ImageId      string `form:"imageId" json:"imageId" binding:"required"`
	} `form:"nodes" json:"nodes" binding:"required"`
}

type Deployment struct {
	Name              string
	Region            string
	TaskDefinitions   []ecs.RegisterTaskDefinitionInput `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
	ClusterDefinition ClusterDefinition                 `from:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       []struct {
		Id   int
		Task string
	} `from:"nodeMapping" json:"nodeMapping" binding:"required"`
}
