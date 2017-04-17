package store

import (
	"fmt"
	"os"
	"path"

	"github.com/hyperpilotio/deployer/common"
	"github.com/spf13/viper"
)

type FileDeployment struct {
	Deployments []*Deployment
}

type File struct {
	Path string
}

func NewFile(config *viper.Viper) *File {
	depStatPath := path.Join(config.GetString("filesPath"), "FileDeployment")
	return &File{
		Path: depStatPath,
	}
}

func (file *File) StoreNewDeployment(deployment *Deployment) error {
	fileDeployment := &FileDeployment{
		Deployments: []*Deployment{},
	}

	if _, err := os.Stat(file.Path); err == nil {
		deployments, err := file.LoadDeployment()
		if err != nil {
			return fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}
		fileDeployment.Deployments = deployments
	}

	deployInfos := map[string]*Deployment{}
	for _, deployment := range fileDeployment.Deployments {
		deployInfos[deployment.Name] = deployment
	}

	newDeployments := []*Deployment{}
	_, ok := deployInfos[deployment.Name]
	if ok {
		deployInfos[deployment.Name] = deployment
		for _, deployInfo := range deployInfos {
			newDeployments = append(newDeployments, deployInfo)
		}
	} else {
		newDeployments = append(newDeployments, deployment)
		fileDeployment.Deployments = newDeployments
	}
	if err := common.WriteObjectToFile(file.Path, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}

func (file *File) LoadDeployment() ([]*Deployment, error) {
	fileDeployment := &FileDeployment{}
	if err := common.LoadFileToObject(file.Path, fileDeployment); err != nil {
		return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	return fileDeployment.Deployments, nil
}
