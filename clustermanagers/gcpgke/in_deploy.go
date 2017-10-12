package gcpgke

import (
	"errors"
	"sort"

	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/go-utils/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

type ClusterNodes []v1.Node

func (c ClusterNodes) Len() int { return len(c) }
func (c ClusterNodes) Less(i, j int) bool {
	return c[i].CreationTimestamp.Before(c[j].CreationTimestamp)
}
func (c ClusterNodes) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

type InClusterGCPDeployer struct {
	GCPDeployer
	ParentClusterId string
}

func NewInClusterDeployer(
	config *viper.Viper,
	deployment *apis.Deployment) (*InClusterGCPDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("Unable to get in cluster kubeconfig: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return nil, errors.New("Unable to create in cluster k8s client: " + err.Error())
	}

	nodes, err := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("Unable to list kubernetes nodes: " + err.Error())
	}

	k8sNodes := ClusterNodes{}
	for _, node := range nodes.Items {
		k8sNodes = append(k8sNodes, node)
	}
	sort.Sort(k8sNodes)

	parentClusterId := ""
	for _, node := range k8sNodes {
		if deployment, ok := node.Labels["hyperpilot/deployment"]; ok {
			parentClusterId = deployment
			break
		}
	}

	if parentClusterId == "" {
		return nil, errors.New("Unable to find deployment name in node labels")
	}

	gcpProfile := &hpgcp.GCPProfile{
		AuthJSONFilePath: config.GetString("gcpServiceAccountJSONFile"),
	}
	projectId, err := gcpProfile.GetProjectId()
	if err != nil {
		return nil, errors.New("Unable to find projectId: " + err.Error())
	}
	gcpProfile.ProjectId = projectId

	clusterId := hpgcp.CreateUniqueClusterId(deployment.Name)
	deployment.Name = clusterId
	deployer := &InClusterGCPDeployer{
		GCPDeployer: GCPDeployer{
			Config: config,
			GCPCluster: &hpgcp.GCPCluster{
				Name:       clusterId,
				Zone:       deployment.Region,
				ClusterId:  clusterId,
				GCPProfile: gcpProfile,
				NodeInfos:  make(map[int]*hpgcp.NodeInfo),
			},
			Deployment:    deployment,
			DeploymentLog: log,
			Services:      make(map[string]ServiceMapping),
			KubeConfig:    kubeConfig,
		},
		ParentClusterId: parentClusterId,
	}

	return deployer, nil
}

// CreateDeployment start a deployment
func (deployer *InClusterGCPDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger

	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	nodePoolIds, err := createNodePools(client, gcpProfile.ProjectId, gcpCluster.Zone,
		deployer.ParentClusterId, deployment, log, true)
	if err != nil {
		return nil, errors.New("Unable to create node pools: " + err.Error())
	}

	log.Infof("GCP CreateDeployment test...%s", nodePoolIds)

	return nil, nil
}

func (deployer *InClusterGCPDeployer) GetCluster() clusters.Cluster {
	return deployer.GCPCluster
}
