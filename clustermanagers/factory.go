package clustermanagers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
)

// Cluster manager specific Deployer, able to deployer containers and services
type Deployer interface {
	CreateDeployment(uploadedFiles map[string]string) (interface{}, error)
	UpdateDeployment() error
	DeleteDeployment() error
	ReloadClusterState(storeInfo interface{}) error
	GetStoreInfo() interface{}
	// TODO(tnachen): Eventually we should support multiple clouds, then we need to abstract AWSCluster
	GetAWSCluster() *aws.AWSCluster
	GetLog() *log.DeploymentLog
	GetScheduler() *job.Scheduler
}

func NewDeployer(
	config *viper.Viper,
	awsProfile *aws.AWSProfile,
	deployType string,
	deployment *apis.Deployment,
	createName bool) (Deployer, error) {

	if createName {
		deployment.Name = createUniqueDeploymentName(deployment.Name)
	}

	switch deployType {
	case "ECS":
		return awsecs.NewDeployer(config, awsProfile, deployment)
	case "K8S":
		return kubernetes.NewDeployer(config, awsProfile, deployment)
	default:
		return nil, errors.New("Unsupported deploy type: " + deployType)
	}
}

func createUniqueDeploymentName(familyName string) string {
	randomId := strings.ToUpper(strings.Split(uuid.NewUUID().String(), "-")[0])
	return fmt.Sprintf("%s-%s", familyName, randomId)
}
