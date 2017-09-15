package gcp

import (
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/gcp"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
)

type GCPDeployer struct {
	Config     *viper.Viper
	GCPCluster *gcp.GCPCluster

	DeploymentLog *log.FileLog
	Deployment    *apis.Deployment
	Scheduler     *job.Scheduler
}
