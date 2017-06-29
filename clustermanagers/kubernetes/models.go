package kubernetes

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	"github.com/spf13/viper"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
)

type ServiceMapping struct {
	NodeId    int
	NodeName  string
	PublicUrl string
}

type K8SDeployer struct {
	Config     *viper.Viper
	AWSCluster *aws.AWSCluster

	DeploymentLog *log.DeploymentLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler

	BastionIp      string
	MasterIp       string
	KubeConfigPath string
	Services       map[string]ServiceMapping
	KubeConfig     *rest.Config
}

type CreateDeploymentResponse struct {
	Name      string                    `json:"name"`
	Services  map[string]ServiceMapping `json:"services"`
	BastionIp string                    `json:"bastionIp"`
	MasterIp  string                    `json:"masterIp"`
}

type DeploymentLoadBalancers struct {
	StackName             string
	ApiServerBalancerName string
	LoadBalancerNames     []string
}

type StoreInfo struct {
	BastionIp string
	MasterIp  string
}

type ClusterInfo struct {
	Nodes      []v1.Node
	Pods       []v1.Pod
	Containers []v1beta1.Deployment
	BastionIp  string
	MasterIp   string
}
