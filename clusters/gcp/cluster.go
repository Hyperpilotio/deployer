package gcp

import (
	"errors"
	"io/ioutil"
	"net/http"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/spf13/viper"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	container "google.golang.org/api/container/v1"
)

type GCPProfile struct {
	UserId             string
	ProjectId          string
	Scopes             []string
	ServiceAccountPath string
}

type NodeInfo struct {
}

// GCPCluster stores the data of a google cloud platform backed cluster
type GCPCluster struct {
	Zone           string
	Name           string
	ClusterVersion string
	GCPProfile     *GCPProfile
	NodeInfos      map[int]*NodeInfo
}

func NewGCPCluster(config *viper.Viper, deployment *apis.Deployment) *GCPCluster {
	return &GCPCluster{
		Zone:           deployment.Region,
		Name:           deployment.Name,
		ClusterVersion: deployment.KubernetesDeployment.GCPDefinition.ClusterVersion,
		GCPProfile: &GCPProfile{
			UserId:    deployment.UserId,
			ProjectId: deployment.KubernetesDeployment.GCPDefinition.ProjectId,
			Scopes: []string{
				container.CloudPlatformScope,
			},
			ServiceAccountPath: config.GetString("gpcServiceAccountJSONFile"),
		},
		NodeInfos: make(map[int]*NodeInfo),
	}
}

func CreateClient(gcpProfile *GCPProfile, Zone string) (*http.Client, error) {
	dat, err := ioutil.ReadFile(gcpProfile.ServiceAccountPath)
	if err != nil {
		return nil, errors.New("Unable to read service account file: " + err.Error())
	}

	conf, err := google.JWTConfigFromJSON(dat, gcpProfile.Scopes...)
	if err != nil {
		return nil, errors.New("Unable to acquire generate config: " + err.Error())
	}

	return conf.Client(oauth2.NoContext), nil
}

func (gcpCluster *GCPCluster) GetClusterType() string {
	return "GCP"
}
