package kubernetes

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/log"
	"github.com/spf13/viper"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

type K8SDeployer struct {
	Config         *viper.Viper
	AWSCluster     *aws.AWSCluster
	DeploymentLog  *log.DeploymentLog
	Deployment     *apis.Deployment
	BastionIp      string
	MasterIp       string
	KubeConfigPath string
	Endpoints      map[string]string
	KubeConfig     *rest.Config
}

type CreateDeploymentResponse struct {
	Name      string            `json:"name"`
	Endpoints map[string]string `json:"endpoints"`
	BastionIp string            `json:"bastionIp"`
	MasterIp  string            `json:"masterIp"`
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
