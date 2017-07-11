package kubernetes

import (
	"errors"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/go-utils/log"
)

type InCluster struct {
	Deployment    *apis.Deployment
	DeploymentLog *log.FileLog
	NodeInfos     map[int]*hpaws.NodeInfo
	State         int
	Created       time.Time
}

func NewInCluster(filesPath string, deployment *apis.Deployment) (*InCluster, error) {
	log, err := log.NewLogger(filesPath, deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	inlCluster := &InCluster{
		Deployment:    deployment,
		DeploymentLog: log,
		NodeInfos:     make(map[int]*hpaws.NodeInfo),
		Created:       time.Now(),
	}

	return inlCluster, nil
}

func (inCluster *InCluster) GetLog() *log.FileLog {
	return inCluster.DeploymentLog
}

func (k8sDeployer *K8SDeployer) CreateInClusterDeployment(
	uploadedFiles map[string]string, inCluster interface{}) (interface{}, error) {
	return nil, nil
}
