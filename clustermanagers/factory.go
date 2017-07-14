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
	"github.com/hyperpilotio/go-utils/log"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"
)

// Cluster manager specific Deployer, able to deployer containers and services
type Deployer interface {
	CreateDeployment(uploadedFiles map[string]string) (interface{}, error)
	UpdateDeployment() error
	DeployExtensions(extensions *apis.Deployment, mergedDeployment *apis.Deployment) error
	DeleteDeployment() error
	ReloadClusterState(storeInfo interface{}) error
	GetStoreInfo() interface{}
	// TODO(tnachen): Eventually we should support multiple clouds, then we need to abstract AWSCluster
	GetAWSCluster() *aws.AWSCluster
	GetLog() *log.FileLog
	GetScheduler() *job.Scheduler
	GetServiceUrl(serviceName string) (string, error)
	GetServiceAddress(serviceName string) (*apis.ServiceAddress, error)
	GetServiceMappings() (map[string]interface{}, error)
}

func NewDeployer(
	config *viper.Viper,
	awsProfile *aws.AWSProfile,
	deployType string,
	deployment *apis.Deployment,
	createName bool,
	originalDeployer Deployer) (Deployer, error) {

	if createName {
		deployment.Name = CreateUniqueDeploymentName(deployment.Name)
	}

	if config.GetBool("inCluster") {
		switch deployType {
		case "K8S":
			k8sDeployer := originalDeployer.(*kubernetes.K8SDeployer)
			originalClusterInfo := &kubernetes.OriginalClusterInfo{
				AWSCluster:     k8sDeployer.AWSCluster,
				BastionIp:      k8sDeployer.BastionIp,
				MasterIp:       k8sDeployer.MasterIp,
				KubeConfigPath: k8sDeployer.KubeConfigPath,
				KubeConfig:     k8sDeployer.KubeConfig,
			}
			return kubernetes.NewInClusterDeployer(config, awsProfile, deployment, originalClusterInfo)
		default:
			return nil, errors.New("Unsupported in cluster deploy type: " + deployType)
		}
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

func CreateUniqueDeploymentName(familyName string) string {
	randomId := strings.ToUpper(strings.Split(uuid.NewUUID().String(), "-")[0])
	return fmt.Sprintf("%s-%s", familyName, randomId)
}
