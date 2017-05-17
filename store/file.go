package store

import (
	"fmt"
	"os"
	"path"
	"sync"

	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"
	"github.com/spf13/viper"
)

type FileDeployment struct {
	Deployments []*awsecs.StoreDeployment
}

type FileAWSProfile struct {
	AWSProfiles []*awsecs.AWSProfile
}

type FileStore struct {
	DeploymentPath  string
	AWSProfilePath  string
	deploymentMutex sync.Mutex
	profileMutex    sync.Mutex
}

func NewFile(config *viper.Viper) (*FileStore, error) {
	deploymentPath := path.Join(config.GetString("filesPath"), "Deployment")
	awsProfilePath := path.Join(config.GetString("filesPath"), "AWSProfile")
	return &FileStore{
		DeploymentPath: deploymentPath,
		AWSProfilePath: awsProfilePath,
	}, nil
}

func (file *FileStore) StoreNewDeployment(deployment *awsecs.StoreDeployment) error {
	file.deploymentMutex.Lock()
	defer file.deploymentMutex.Unlock()

	deployInfos, err := file.getDeployInfos()
	if err != nil {
		return fmt.Errorf("Unable to get deployment info: %s", err.Error())
	}
	deployInfos[deployment.Name] = deployment

	deployments := []*awsecs.StoreDeployment{}
	for _, deployInfo := range deployInfos {
		deployments = append(deployments, deployInfo)
	}

	fileDeployment := &FileDeployment{
		Deployments: deployments,
	}

	if err := common.WriteObjectToFile(file.DeploymentPath, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}

func (file *FileStore) LoadDeployments() ([]*awsecs.StoreDeployment, error) {
	file.deploymentMutex.Lock()
	defer file.deploymentMutex.Unlock()

	fileDeployment := &FileDeployment{}
	if _, err := os.Stat(file.DeploymentPath); err == nil {
		if err := common.LoadFileToObject(file.DeploymentPath, fileDeployment); err != nil {
			return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}
	}

	return fileDeployment.Deployments, nil
}

func (file *FileStore) DeleteDeployment(deploymentName string) error {
	file.deploymentMutex.Lock()
	defer file.deploymentMutex.Unlock()

	deployInfos, err := file.getDeployInfos()
	if err != nil {
		return fmt.Errorf("Unable to get deployment info: %s", err.Error())
	}
	delete(deployInfos, deploymentName)

	deployments := []*awsecs.StoreDeployment{}
	for _, deployInfo := range deployInfos {
		deployments = append(deployments, deployInfo)
	}

	fileDeployment := &FileDeployment{
		Deployments: deployments,
	}

	if err := common.WriteObjectToFile(file.DeploymentPath, fileDeployment); err != nil {
		return fmt.Errorf("Unable to store deployment status: %s", err.Error())
	}

	return nil
}

func (file *FileStore) StoreNewAWSProfile(awsProfile *awsecs.AWSProfile) error {
	file.profileMutex.Lock()
	defer file.profileMutex.Unlock()

	awsProfileInfos, err := file.getAWSProfileInfos()
	if err != nil {
		return fmt.Errorf("Unable to get aws profile info: %s", err.Error())
	}
	awsProfileInfos[awsProfile.UserId] = awsProfile

	awsProfiles := []*awsecs.AWSProfile{}
	for _, awsProfileInfo := range awsProfileInfos {
		awsProfiles = append(awsProfiles, awsProfileInfo)
	}

	fileAWSProfile := &FileAWSProfile{
		AWSProfiles: awsProfiles,
	}

	if err := common.WriteObjectToFile(file.AWSProfilePath, fileAWSProfile); err != nil {
		return fmt.Errorf("Unable to store aws profile: %s", err.Error())
	}

	return nil
}

func (file *FileStore) LoadAWSProfiles() ([]*awsecs.AWSProfile, error) {
	file.profileMutex.Lock()
	defer file.profileMutex.Unlock()

	fileAWSProfile := &FileAWSProfile{}
	if _, err := os.Stat(file.AWSProfilePath); err == nil {
		if err := common.LoadFileToObject(file.AWSProfilePath, fileAWSProfile); err != nil {
			return nil, fmt.Errorf("Unable to load aws profile: %s", err.Error())
		}
	}

	return fileAWSProfile.AWSProfiles, nil
}

func (file *FileStore) LoadAWSProfile(userId string) (*awsecs.AWSProfile, error) {
	if userId == "" {
		return nil, fmt.Errorf("Unable to find userId when load aws profile")
	}

	awsProfiles, err := file.LoadAWSProfiles()
	if err != nil {
		return nil, err
	}

	findUser := false
	userAwsProfile := &awsecs.AWSProfile{}
	for _, awsProfile := range awsProfiles {
		if awsProfile.UserId == userId {
			userAwsProfile = awsProfile
			findUser = true
			break
		}
	}

	if !findUser {
		return nil, fmt.Errorf("Unable to find %s aws profile", userId)
	}

	return userAwsProfile, nil
}

func (file *FileStore) DeleteAWSProfile(userId string) error {
	awsProfileInfos, err := file.getAWSProfileInfos()
	if err != nil {
		return fmt.Errorf("Unable to get aws profile info: %s", err.Error())
	}
	delete(awsProfileInfos, userId)

	awsProfiles := []*awsecs.AWSProfile{}
	for _, awsProfileInfo := range awsProfileInfos {
		awsProfiles = append(awsProfiles, awsProfileInfo)
	}

	fileAWSProfile := &FileAWSProfile{
		AWSProfiles: awsProfiles,
	}

	file.profileMutex.Lock()
	defer file.profileMutex.Unlock()

	if err := common.WriteObjectToFile(file.AWSProfilePath, fileAWSProfile); err != nil {
		return fmt.Errorf("Unable to store aws profile: %s", err.Error())
	}

	return nil
}

func (file *FileStore) getDeployInfos() (map[string]*awsecs.StoreDeployment, error) {
	deployInfos := map[string]*awsecs.StoreDeployment{}
	if _, err := os.Stat(file.DeploymentPath); err == nil {
		deployments, err := file.LoadDeployments()
		if err != nil {
			return nil, fmt.Errorf("Unable to load deployment status: %s", err.Error())
		}

		for _, deployment := range deployments {
			deployInfos[deployment.Name] = deployment
		}
	}

	return deployInfos, nil
}

func (file *FileStore) getAWSProfileInfos() (map[string]*awsecs.AWSProfile, error) {
	awsProfileInfos := map[string]*awsecs.AWSProfile{}
	if _, err := os.Stat(file.AWSProfilePath); err == nil {
		awsProfiles, err := file.LoadAWSProfiles()
		if err != nil {
			return nil, fmt.Errorf("Unable to load aws profile: %s", err.Error())
		}

		for _, awsProfile := range awsProfiles {
			awsProfileInfos[awsProfile.UserId] = awsProfile
		}
	}

	return awsProfileInfos, nil
}
