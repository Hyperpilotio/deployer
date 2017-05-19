package awsecs

import (
	"sync"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	"github.com/spf13/viper"
)

type ECSDeployer struct {
	Config     *viper.Viper
	AWSCluster *aws.AWSCluster

	Deployment    *apis.Deployment
	DeploymentLog *log.DeploymentLog
	Scheduler     *job.Scheduler

	mutex sync.Mutex
}

type ClusterInfo struct {
	ContainerInstances []*ecs.ContainerInstance
	Reservations       []*ec2.Reservation
	Tasks              []*ecs.Task
}
