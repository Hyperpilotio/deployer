package deploy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/hyperpilotio/deployer/log"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
)

type Deployer interface {
	CreateDeployment(uploadedFiles map[string]string) error
	UpdateDeployment() error
	DeleteDeployment() error

	NewShutDownScheduler(scheduleRunTime string) error

	GetDeploymentInfo() *awsecs.DeploymentInfo
	GetKubeConfigPath() string
	GetLog() *log.DeploymentLog
	GetScheduler() *job.Scheduler
}

func NewDeployer(config *viper.Viper, awsProfiles map[string]*awsecs.AWSProfile, deployment *apis.Deployment) (Deployer, error) {
	deployType := ""
	if deployment.KubernetesDeployment != nil {
		deployType = "K8S"
	} else {
		deployType = "ECS"
	}

	deployment.Name = createUniqueDeploymentName(deployment.Name)

	switch deployType {
	case "ECS":
		return awsecs.NewDeployer(config, awsProfiles, deployment)
	case "K8S":
		return kubernetes.NewDeployer(config, awsProfiles, deployment)
	default:
		return nil, errors.New("Unsupported deploy type: " + deployType)
	}
}

func createUniqueDeploymentName(familyName string) string {
	randomId := strings.ToUpper(strings.Split(uuid.NewUUID().String(), "-")[0])
	return fmt.Sprintf("%s-%s", familyName, randomId)
}
