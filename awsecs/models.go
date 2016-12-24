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
	Scale             int64                             `json:"scale"`
	TaskDefinitions   []ecs.RegisterTaskDefinitionInput `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
	AllowedPorts      []int                             `form:"allowedPorts" json:"allowedPorts"`
	ClusterDefinition ClusterDefinition                 `form:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       []struct {
		Id   int    `json:"id"`
		Task string `json:"task"`
	} `form:"nodeMapping" json:"nodeMapping" binding:"required"`
	IamRole `form:"iamRole" json:"iamRole" binding:"required"`
}

// IamRole store the information of iam role
type IamRole struct {
	PolicyDocument string `form:"policyDocument" json:"policyDocument"`
}
