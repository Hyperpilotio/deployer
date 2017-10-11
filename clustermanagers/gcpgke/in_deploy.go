package gcpgke

import (
	"errors"
	"sort"

	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
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

	clusterId := ""
	for _, node := range k8sNodes {
		if deployment, ok := node.Labels["hyperpilot/deployment"]; ok {
			clusterId = deployment
			break
		}
	}

	if clusterId == "" {
		return nil, errors.New("Unable to find deployment name in node labels")
	}

	deployer := &InClusterGCPDeployer{
		GCPDeployer: GCPDeployer{
			Config: config,
			GCPCluster: &hpgcp.GCPCluster{
				Name:      clusterId,
				Zone:      deployment.Region,
				ClusterId: clusterId,
				GCPProfile: &hpgcp.GCPProfile{
					AuthJSONFilePath: config.GetString("gcpServiceAccountJSONFile"),
				},
				NodeInfos: make(map[int]*hpgcp.NodeInfo),
			},
			Deployment:    deployment,
			DeploymentLog: log,
			Services:      make(map[string]ServiceMapping),
			KubeConfig:    kubeConfig,
		},
	}

	return deployer, nil
}

// CreateDeployment start a deployment
func (deployer *InClusterGCPDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	log := deployer.GetLog().Logger
	gcpCluster := deployer.GCPCluster

	client, err := hpgcp.CreateClient(gcpCluster.GCPProfile)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	log.Infof("GCP CreateDeployment test...", client)

	return nil, nil
}
