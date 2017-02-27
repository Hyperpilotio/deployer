package apis

import (
	"fmt"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"

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

// Service return a string with suffix "-service"
func (mapping NodeMapping) Service() string {
	return mapping.Task + "-service"
}

// ImageIdAttribute return imageId for putAttribute function
func (mapping NodeMapping) ImageIdAttribute() string {
	return fmt.Sprintf("imageId-%d", mapping.Id)
}

// ECSDeployment storing the information of a ECS deployment
type ECSDeployment struct {
	TaskDefinitions []ecs.RegisterTaskDefinitionInput `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
}

// KubernetesDeployment storing the information of a Kubernetes deployment
type KubernetesDeployment struct {
	Kubernetes []struct {
		Deployment v1beta1.Deployment `form:"deployment" json:"deployment" binding:"required"`
		Service    v1.Service         `form:"service" json:"service"`
		Family     string             `form:"family" json:"family" binding:"required"`
	} `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
}

type Deployment struct {
	Name   string `form:"name" json:"name" binding:"required"`
	Region string `form:"region" json:"region" binding:"required"`
	Files  []struct {
		FileId string `json:"fileId"`
		Path   string `json:"path"`
	} `form:"files" json:"files"`
	AllowedPorts      []int             `form:"allowedPorts" json:"allowedPorts"`
	ClusterDefinition ClusterDefinition `form:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       []NodeMapping     `form:"nodeMapping" json:"nodeMapping" binding:"required"`
	IamRole           `form:"iamRole" json:"iamRole" binding:"required"`

	*ECSDeployment        `form:"ecs" json:"ecs,omitempty"`
	*KubernetesDeployment `form:"kubernetes" json:"kubernetes,omitempty"`
}

// IamRole store the information of iam role
type IamRole struct {
	PolicyDocument string `form:"policyDocument" json:"policyDocument"`
}
