package gcpgke

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
	"k8s.io/client-go/rest"
)

type ServiceMapping struct {
	NodeId    int    `json:"nodeId"`
	NodeName  string `json:"nodeName"`
	PublicUrl string `json:"publicUrl"`
}

type GCPDeployer struct {
	Config     *viper.Viper
	GCPCluster *gcp.GCPCluster

	DeploymentLog *log.FileLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler

	KubeConfigPath string
	KubeConfig     *rest.Config
	Services       map[string]ServiceMapping
}

type CreateDeploymentResponse struct {
	Name     string                    `json:"name"`
	Services map[string]ServiceMapping `json:"services"`
}
