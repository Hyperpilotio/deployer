package store

import (
	"errors"
	"strings"

	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/spf13/viper"
)

type StoreDeployment struct {
	Name          string
	Region        string
	Type          string
	Status        string
	ECSDeployment *awsecs.StoreDeployment
	K8SDeployment *kubernetes.StoreDeployment
}

type Store interface {
	StoreNewDeployment(storeDeployment *StoreDeployment) error
	LoadDeployment() ([]*StoreDeployment, error)
	DeleteDeployment(deploymentName string) error
}

func NewStore(config *viper.Viper) (Store, error) {
	storeType := strings.ToLower(config.GetString("store.type"))
	switch storeType {
	case "simpledb":
		return NewSimpleDB(config)
	case "file":
		return NewFile(config), nil
	default:
		return nil, errors.New("Unsupported store type: " + storeType)
	}
}
