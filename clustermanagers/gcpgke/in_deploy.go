package gcpgke

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	k8sUtil "github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
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
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

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
	if err := deployInCluster(deployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	return nil, nil
}

func deployInCluster(deployer *InClusterGCPDeployer, uploadedFiles map[string]string) error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	nodePoolIds, err := createNodePools(client, gcpProfile.ProjectId, gcpCluster.Zone,
		deployer.ParentClusterId, deployment, log, true)
	if err != nil {
		return errors.New("Unable to create node pools: " + err.Error())
	}

	if err := populateNodeInfos(client, gcpProfile.ProjectId, gcpCluster.Zone,
		deployer.ParentClusterId, nodePoolIds, gcpCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	nodeNames := []string{}
	for _, nodeInfo := range gcpCluster.NodeInfos {
		nodeNames = append(nodeNames, nodeInfo.Instance.Name)
	}
	if err := k8sUtil.WaitUntilKubernetesNodeExists(k8sClient, nodeNames, time.Duration(2)*time.Minute, log); err != nil {
		// deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		// deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := deployer.deployKubernetesObjects(k8sClient, false); err != nil {
		// deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	if err := tagFirewallIngressRules(client, gcpCluster, deployment, log); err != nil {
		// deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to tag firewall ingress rules: " + err.Error())
	}
	deployer.recordPublicEndpoints()

	return nil
}

func (deployer *InClusterGCPDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset, skipDelete bool) error {
	log := deployer.GetLog().Logger
	existingNamespaces, namespacesErr := k8sUtil.GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	namespace := deployer.getNamespace()
	if err := k8sUtil.CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
		return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
	}

	if err := k8sUtil.CreateSecretsByNamespace(k8sClient, namespace, deployer.Deployment); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	// TODO incluster ServiceAccount now is nil, need to restore
	// 1)tag serviceAccount to compute metadata
	// 2)get serviceAccount from compute metadata
	userName := strings.ToLower(deployer.GCPCluster.GCPProfile.ServiceAccount)
	if err := k8sUtil.DeployServices(k8sClient, deployer.Deployment, namespace,
		existingNamespaces, userName, log); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func (deployer *InClusterGCPDeployer) getNamespace() string {
	return strings.ToLower(deployer.GCPCluster.Name)
}
