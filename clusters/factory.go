package clusters

import (
	"github.com/golang/glog"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/spf13/viper"
)

type Cluster interface {
	GetClusterType() string
}

func NewCluster(
	config *viper.Viper,
	deployType string,
	awsProfile *hpaws.AWSProfile,
	deployment *apis.Deployment) Cluster {
	switch deployType {
	case "ECS", "K8S":
		return hpaws.NewAWSCluster(deployment.Name, deployment.Region, awsProfile)
	case "GCP":
		return hpgcp.NewGCPCluster(config, deployment)
	default:
		glog.Errorf("Unsupported deploy type: " + deployType)
		return nil
	}
}
