package gcpgke

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
	"k8s.io/client-go/rest"
)

type GCPDeployer struct {
	Config     *viper.Viper
	GCPCluster *gcp.GCPCluster

	DeploymentLog *log.FileLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler

	KubeConfigPath string
	KubeConfig     *rest.Config
	Services       map[string]kubernetes.ServiceMapping
}

type StoreInfo struct {
	ClusterId string
}

type CreateDeploymentResponse struct {
	Name      string                               `json:"name"`
	ClusterId string                               `json:"clusterId"`
	Services  map[string]kubernetes.ServiceMapping `json:"services"`
}
