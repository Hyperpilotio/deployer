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

func NewFile(config *viper.Viper) *File {
	depStatPath := path.Join(config.GetString("filesPath"), "FileDeployment")
	return &File{
		Path: depStatPath,
	}
}

func (file *File) StoreNewDeployment(deployment *StoreDeployment) error {
	fileDeployment := &FileDeployment{
		Deployments: []*StoreDeployment{},
	}

	if _, err := os.Stat(file.Path); err == nil {
		deployments, err := file.LoadDeployment()
		if err != nil {
			return fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}
		fileDeployment.Deployments = deployments
	}

	deployInfos := map[string]*StoreDeployment{}
	for _, deployment := range fileDeployment.Deployments {
		deployInfos[deployment.Name] = deployment
	}

	newDeployments := []*StoreDeployment{}
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

func (file *File) LoadDeployment() ([]*StoreDeployment, error) {
	fileDeployment := &FileDeployment{}
	if err := common.LoadFileToObject(file.Path, fileDeployment); err != nil {
		return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	return fileDeployment.Deployments, nil
}

func (file *File) DeleteDeployment(deploymentName string) error {
	fileDeployment := &FileDeployment{
		Deployments: []*StoreDeployment{},
	}

	if _, err := os.Stat(file.Path); err == nil {
		deployments, err := file.LoadDeployment()
		if err != nil {
			return fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}
		fileDeployment.Deployments = deployments
	}

	deployInfos := map[string]*StoreDeployment{}
	for _, deployment := range fileDeployment.Deployments {
		if deployment.Name == deploymentName {
			continue
		}
		deployInfos[deployment.Name] = deployment
	}

	newDeployments := []*StoreDeployment{}
	for _, deployInfo := range deployInfos {
		newDeployments = append(newDeployments, deployInfo)
	}

	if err := common.WriteObjectToFile(file.Path, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}
