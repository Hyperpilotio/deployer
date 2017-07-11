package awsecs

import (
	"errors"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/go-utils/log"
)

type InCluster struct {
}

func NewInCluster(filesPath string, deployment *apis.Deployment) (*InCluster, error) {
	return nil, errors.New("Unimplemented")
}

func (inCluster *InCluster) GetLog() *log.FileLog {
	return nil
}

func (ecsDeployer *ECSDeployer) CreateInClusterDeployment(
	uploadedFiles map[string]string, inCluster interface{}) (interface{}, error) {
	return nil, errors.New("Unimplemented")
}
