package kubernetes

import (
	"errors"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/go-utils/log"
)

type InClusterK8SDeployer struct {
	Deployment    *apis.Deployment
	DeploymentLog *log.FileLog
	NodeInfos     map[int]*hpaws.NodeInfo
	Created       time.Time
}

func NewInClusterDeployer(filesPath string, deployment *apis.Deployment) (*InClusterK8SDeployer, error) {
	log, err := log.NewLogger(filesPath, deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	inlCluster := &InClusterK8SDeployer{
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
