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
	GetKeyMaterial() string
	ReloadKeyPair(keyMaterial string) error
}

type UserProfile interface {
	GetGCPProfile() *hpgcp.GCPProfile
	GetAWSProfile() *hpaws.AWSProfile
}

func NewCluster(
	config *viper.Viper,
	deployType string,
	userProfile UserProfile,
	deployment *apis.Deployment) Cluster {
	switch deployType {
	case "ECS", "K8S":
		awsCluster := hpaws.NewAWSCluster(deployment.Name, deployment.Region)
		awsCluster.AWSProfile = userProfile.GetAWSProfile()
		return awsCluster
	case "GCP":
		gcpCluster := hpgcp.NewGCPCluster(config, deployment)
		gcpProfile := userProfile.GetGCPProfile()
		gcpCluster.GCPProfile = gcpProfile
		return gcpCluster
	default:
		glog.Errorf("Unsupported deploy type: " + deployType)
		return nil
	}
}
