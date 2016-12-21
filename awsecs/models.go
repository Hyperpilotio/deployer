package awsecs

import (
	"github.com/aws/aws-sdk-go/service/ecs"
)

// ClusterDefinition store the information of cluster
type ClusterDefinition struct {
	Nodes []struct {
		Id           int    `form:"id" json:"id" binding:"required"`
		InstanceType string `form:"instanceType" json:"instanceType" binding:"required"`
		ImageId      string `form:"imageId" json:"imageId" binding:"required"`
	} `form:"nodes" json:"nodes" binding:"required"`
}

// Deployment store the information of a deployment
type Deployment struct {
	Name              string                            `json:"name"`
	Region            string                            `json:"region"`
	TaskDefinitions   []ecs.RegisterTaskDefinitionInput `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
	ClusterDefinition ClusterDefinition                 `from:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       []struct {
		Id   int    `json:"id"`
		Task string `json:"task"`
	} `from:"nodeMapping" json:"nodeMapping" binding:"required"`
}
