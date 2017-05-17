package store

import (
	"errors"
	"strings"

	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/spf13/viper"
)

type Store interface {
	StoreNewDeployment(storeDeployment *awsecs.StoreDeployment) error
	LoadDeployments() ([]*awsecs.StoreDeployment, error)
	DeleteDeployment(deploymentName string) error
	StoreNewAWSProfile(awsProfile *awsecs.AWSProfile) error
	LoadAWSProfiles() ([]*awsecs.AWSProfile, error)
	LoadAWSProfile(userId string) (*awsecs.AWSProfile, error)
	DeleteAWSProfile(userId string) error
}

func NewStore(config *viper.Viper) (Store, error) {
	storeType := strings.ToLower(config.GetString("store.type"))
	switch storeType {
	case "simpledb":
		return NewSimpleDB(config)
	case "file":
		return NewFile(config)
	default:
		return nil, errors.New("Unsupported store type: " + storeType)
	}
}
