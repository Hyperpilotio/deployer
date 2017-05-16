package kubernetes

import (
	"sync"

	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	"github.com/spf13/viper"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

type K8SDeployer struct {
	Config *viper.Viper
	Type   string

	DeploymentInfo *awsecs.DeploymentInfo
	DeploymentLog  *log.DeploymentLog
	Scheduler      *job.Scheduler

	mutex sync.Mutex
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

type ClusterInfo struct {
	Nodes      []v1.Node
	Pods       []v1.Pod
	Containers []v1beta1.Deployment
	BastionIp  string
	MasterIp   string
}
