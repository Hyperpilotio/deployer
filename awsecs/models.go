package awsecs

import (
	"github.com/aws/aws-sdk-go/service/ecs"
)

// ClusterDefinition storing the information of a cluster
type ClusterDefinition struct {
	Nodes []struct {
		Id           int    `form:"id" json:"id" binding:"required"`
		InstanceType string `form:"instanceType" json:"instanceType" binding:"required"`
	} `form:"nodes" json:"nodes" binding:"required"`
}

type NodeMapping struct {
	Count int    `json:"count"`
	Id    int    `json:"id"`
	Task  string `json:"task"`
}

// Deployment storing the information of a deployment
type Deployment struct {
	Name   string `form:"name" json:"name" binding:"required"`
	Region string `form:"region" json:"region" binding:"required"`
	Scale  int64  `json:"scale"`
	Files  []struct {
		FileId string `json:"fileId"`
		Path   string `json:"path"`
	} `form:"files" json:"files"`
	TaskDefinitions   []ecs.RegisterTaskDefinitionInput `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
	AllowedPorts      []int                             `form:"allowedPorts" json:"allowedPorts"`
	ClusterDefinition ClusterDefinition                 `form:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       []NodeMapping                     `form:"nodeMapping" json:"nodeMapping" binding:"required"`
	IamRole           `form:"iamRole" json:"iamRole" binding:"required"`
}

// IamRole store the information of iam role
type IamRole struct {
	PolicyDocument string `form:"policyDocument" json:"policyDocument"`
}
