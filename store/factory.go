package store

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/spf13/viper"
)

type Deployment struct {
	Name          string
	Region        string
	BastionIp     string
	MasterIp      string
	KeyName       string
	KeyMaterial_1 string
	KeyMaterial_2 string
	Status        string
}

type Store interface {
	StoreNewDeployment(deployment *Deployment) error
	LoadDeployment() ([]*Deployment, error)
	// UpdateDeployment() error
	// DeleteDeployment() error
}

func NewDeployment(deployedCluster *awsecs.DeployedCluster, k8sDeployment *kubernetes.KubernetesDeployment) *Deployment {
	deploymentName := deployedCluster.Name
	deployment := &Deployment{
		Name:          deploymentName,
		Region:        deployedCluster.Deployment.Region,
		BastionIp:     "",
		MasterIp:      "",
		KeyName:       aws.StringValue(deployedCluster.KeyPair.KeyName),
		KeyMaterial_1: aws.StringValue(deployedCluster.KeyPair.KeyMaterial)[:1024],
		KeyMaterial_2: aws.StringValue(deployedCluster.KeyPair.KeyMaterial)[1024:],
	}

	if k8sDeployment != nil {
		bastionIp := k8sDeployment.BastionIp
		masterIp := k8sDeployment.MasterIp

		deployment.BastionIp = bastionIp
		deployment.MasterIp = masterIp
	}

	return deployment
}

func Initialize(config *viper.Viper) (Store, error) {
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

func ReloadClusterState(config *viper.Viper, deployments []*Deployment, deployedClusters map[string]*awsecs.DeployedCluster, kubernetesClusters *kubernetes.KubernetesClusters) error {
	for _, storeDeployment := range deployments {
		deploymentName := storeDeployment.Name
		deployment := &apis.Deployment{
			Name:   deploymentName,
			Region: storeDeployment.Region,
		}
		deployedCluster := awsecs.NewDeployedCluster(deployment)

		// Reload keypair
		if err := awsecs.ReloadKeyPair(config, deployedCluster); err != nil {
			return fmt.Errorf("Unable to load %s keyPair: %s", deploymentName, err.Error())
		}

		deployedCluster.KeyPair.KeyMaterial = aws.String(storeDeployment.KeyMaterial_1 + storeDeployment.KeyMaterial_2)

		if (storeDeployment.BastionIp != "") && (storeDeployment.MasterIp != "") {
			// Deployment type is kubernetes
			deployedCluster.Deployment.KubernetesDeployment = &apis.KubernetesDeployment{}
			k8sDeployment := &kubernetes.KubernetesDeployment{
				BastionIp:       storeDeployment.BastionIp,
				MasterIp:        storeDeployment.MasterIp,
				DeployedCluster: deployedCluster,
			}

			if err := k8sDeployment.DownloadKubeConfig(); err != nil {
				return fmt.Errorf("Unable to download %s kubeconfig: %s", deploymentName, err.Error())
			} else {
				glog.Infof("Downloaded %s kube config at %s", deploymentName, k8sDeployment.KubeConfigPath)
				if kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployment.KubeConfigPath); err != nil {
					return fmt.Errorf("Unable to parse %s kube config: %s", deploymentName, err.Error())
				} else {
					k8sDeployment.KubeConfig = kubeConfig
				}
			}
			kubernetesClusters.Clusters[deploymentName] = k8sDeployment
		} else {
			//  Deployment type is awsecs
			deployedCluster.Deployment.ECSDeployment = &apis.ECSDeployment{}
			if err := awsecs.ReloadInstanceIds(config, deployedCluster); err != nil {
				return fmt.Errorf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
			}
		}
		deployedClusters[deploymentName] = deployedCluster
	}

	return nil
}
