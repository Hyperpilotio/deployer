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
	Id   int    `json:"id"`
	Task string `json:"task"`
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

type KubernetesTask struct {
	DaemonSet  *v1beta1.DaemonSet  `form:"daemonset" json:"daemonset,omitempty"`
	Deployment *v1beta1.Deployment `form:"deployment" json:"deployment,omitempty"`
	Family     string              `form:"family" json:"family" binding:"required"`

	// Type of each port opened by a container: 0 - private, 1 - public
	PortTypes []int `form:"portTypes" json:"portTypes"`
}

// KubernetesDeployment storing the information of a Kubernetes deployment
type KubernetesDeployment struct {
	Kubernetes          []KubernetesTask `form:"taskDefinitions" json:"taskDefinitions" binding:"required"`
	Secrets             []v1.Secret      `form:"secrets" json:"secrets"`
	SkipDeleteOnFailure bool             `form:"skipdDeleteOnFailure" json:"skipDeleteOnFailure"`
}

type Deployment struct {
	UserId string `form:"userId" json:"userId" binding:"required"`
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

	ShutDownTime string `form:"shutDownTime" json:"shutDownTime,omitempty"`
}

// IamRole store the information of iam role
type IamRole struct {
	PolicyDocument string `form:"policyDocument" json:"policyDocument"`
}
