package awsecs

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
)

type ECSDeployer struct {
	Config     *viper.Viper
	AWSCluster *aws.AWSCluster

	Deployment    *apis.Deployment
	DeploymentLog *log.FileLog
	Scheduler     *job.Scheduler
}

type ClusterInfo struct {
	ContainerInstances []*ecs.ContainerInstance
	Reservations       []*ec2.Reservation
	Tasks              []*ecs.Task
}
