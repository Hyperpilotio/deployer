package awsk8s

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"

	"k8s.io/client-go/rest"
)

type K8SDeployer struct {
	Config     *viper.Viper
	AWSCluster *aws.AWSCluster

	DeploymentLog *log.FileLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler

	BastionIp              string
	MasterIp               string
	KubeConfigPath         string
	Services               map[string]kubernetes.ServiceMapping
	KubeConfig             *rest.Config
	VpcPeeringConnectionId string
}

type CreateDeploymentResponse struct {
	Name      string                               `json:"name"`
	Services  map[string]kubernetes.ServiceMapping `json:"services"`
	BastionIp string                               `json:"bastionIp"`
	MasterIp  string                               `json:"masterIp"`
}

type DeploymentLoadBalancers struct {
	StackName             string
	ApiServerBalancerName string
	LoadBalancerNames     []string
}

type StoreInfo struct {
	BastionIp              string
	MasterIp               string
	VpcPeeringConnectionId string
}
