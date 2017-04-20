package store

import (
	"fmt"
	"os"
	"path"

	"github.com/hyperpilotio/deployer/common"
	"github.com/spf13/viper"
)

type FileDeployment struct {
	Deployments []*StoreDeployment
}

type File struct {
	Path string
}

func NewFile(config *viper.Viper) (*File, error) {
	depStatPath := path.Join(config.GetString("filesPath"), "Deployment")
	return &File{
		Path: depStatPath,
	}, nil
}

func (file *File) StoreNewDeployment(deployment *StoreDeployment) error {
	deployInfos, err := file.getDeployInfos()
	if err != nil {
		return fmt.Errorf("Unable to get deployment info: %s", err.Error())
	}
	deployInfos[deployment.Name] = deployment

	deployments := []*StoreDeployment{}
	for _, deployInfo := range deployInfos {
		deployments = append(deployments, deployInfo)
	}

	fileDeployment := &FileDeployment{
		Deployments: deployments,
	}

	if err := common.WriteObjectToFile(file.Path, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}

func (file *File) LoadDeployment() ([]*StoreDeployment, error) {
	fileDeployment := &FileDeployment{}
	if _, err := os.Stat(file.Path); err == nil {
		if err := common.LoadFileToObject(file.Path, fileDeployment); err != nil {
			return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}
	}

	return fileDeployment.Deployments, nil
}

func (file *File) DeleteDeployment(deploymentName string) error {
	deployInfos, err := file.getDeployInfos()
	if err != nil {
		return fmt.Errorf("Unable to get deployment info: %s", err.Error())
	}
	delete(deployInfos, deploymentName)

	deployments := []*StoreDeployment{}
	for _, deployInfo := range deployInfos {
		deployments = append(deployments, deployInfo)
	}

	fileDeployment := &FileDeployment{
		Deployments: deployments,
	}

	if err := common.WriteObjectToFile(file.Path, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}

func (file *File) getDeployInfos() (map[string]*StoreDeployment, error) {
	deployInfos := map[string]*StoreDeployment{}
	if _, err := os.Stat(file.Path); err == nil {
		deployments, err := file.LoadDeployment()
		if err != nil {
			return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}

		for _, deployment := range deployments {
			deployInfos[deployment.Name] = deployment
		}
	}

	return deployInfos, nil
}
