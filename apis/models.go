package apis

import (
	"fmt"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"

	"github.com/aws/aws-sdk-go/service/ecs"
)

type ClusterNode struct {
	Id           int    `form:"id" json:"id" binding:"required"`
	InstanceType string `form:"instanceType" json:"instanceType" binding:"required"`
}

// ClusterDefinition storing the information of a cluster
type ClusterDefinition struct {
	Nodes []ClusterNode `form:"nodes" json:"nodes" binding:"required"`
}

type GCPDefinition struct {
	ClusterVersion string `form:"clusterVersion" json:"clusterVersion"`
	ProjectId      string `form:"projectId" json:"projectId" binding:"required"`
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
	GCPDefinition       GCPDefinition    `form:"gcpDefinition" json:"gcpDefinition"`
}

type NodeMappings []NodeMapping

func (d NodeMappings) Len() int { return len(d) }
func (d NodeMappings) Less(i, j int) bool {
	return d[i].Id < d[j].Id
}
func (d NodeMappings) Swap(i, j int) { d[i], d[j] = d[j], d[i] }

type VPCPeering struct {
	TargetOwnerId string `json:"targetOwnerId"`
	TargetVpcId   string `json:"targetVpcId"`
}

type Deployment struct {
	UserId string `form:"userId" json:"userId"`
	Name   string `form:"name" json:"name" binding:"required"`
	Region string `form:"region" json:"region" binding:"required"`
	Files  []struct {
		FileId string `json:"fileId"`
		Path   string `json:"path"`
	} `form:"files" json:"files"`
	AllowedPorts      []int             `form:"allowedPorts" json:"allowedPorts"`
	ClusterDefinition ClusterDefinition `form:"clusterDefinition" json:"clusterDefinition" binding:"required"`
	NodeMapping       NodeMappings      `form:"nodeMapping" json:"nodeMapping" binding:"required"`
	IamRole           `form:"iamRole" json:"iamRole" binding:"required"`

	*ECSDeployment        `form:"ecs" json:"ecs,omitempty"`
	*KubernetesDeployment `form:"kubernetes" json:"kubernetes,omitempty"`

	*VPCPeering `json:"vpcPeering,omitempty"`

	ShutDownTime string `form:"shutDownTime" json:"shutDownTime,omitempty"`
}

// IamRole store the information of iam role
type IamRole struct {
	PolicyDocument string `form:"policyDocument" json:"policyDocument"`
}

// ServiceAddress object that stores the information of service container
type ServiceAddress struct {
	Host string `bson:"host,omitempty" json:"host,omitempty"`
	Port int32  `bson:"port,omitempty" json:"port,omitempty"`
}
