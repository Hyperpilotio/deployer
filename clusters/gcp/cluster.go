package gcp

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/spf13/viper"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
)

type GCPProfile struct {
	UserId             string
	ProjectId          string
	Scopes             []string
	ServiceAccountPath string
}

type NodeInfo struct {
	Instance      *compute.Instance
	Arn           string
	PublicDnsName string
	PrivateIp     string
}

// GCPCluster stores the data of a google cloud platform backed cluster
type GCPCluster struct {
	Zone           string
	ClusterId      string
	ClusterVersion string
	GCPProfile     *GCPProfile
	NodeInfos      map[int]*NodeInfo
}

func NewGCPCluster(config *viper.Viper, deployment *apis.Deployment) *GCPCluster {
	clusterId := CreateUniqueClusterId(deployment.Name)
	return &GCPCluster{
		Zone:           deployment.Region,
		ClusterId:      clusterId,
		ClusterVersion: deployment.KubernetesDeployment.GCPDefinition.ClusterVersion,
		GCPProfile: &GCPProfile{
			UserId:    deployment.UserId,
			ProjectId: deployment.KubernetesDeployment.GCPDefinition.ProjectId,
			Scopes: []string{
				container.CloudPlatformScope,
				compute.ComputeScope,
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

func CreateUniqueClusterId(deploymentName string) string {
	randomId := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("%s-%s", strings.Split(deploymentName, "-")[0], randomId)
}
