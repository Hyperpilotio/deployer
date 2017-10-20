package gcpgke

import (
	"errors"
	"fmt"
	"io/ioutil"
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

	b, err := ioutil.ReadFile(config.GetString("gcpServiceAccountJSONFile"))
	if err != nil {
		return nil, errors.New("Unable to read service account file: " + err.Error())
	}

	gcpProfile := &hpgcp.GCPProfile{
		AuthJSONFileContent: string(b),
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

	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	serviceAccount, err := findServiceAccount(client, projectId, log.Logger)
	if err != nil {
		return nil, errors.New("Unable to find serviceAccount: " + err.Error())
	}
	gcpProfile.ServiceAccount = serviceAccount
	log.Logger.Infof("Reload in-cluster serviceAccount: %s", gcpProfile.ServiceAccount)

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

	nodePoolIds, err := CreateNodePools(client, gcpProfile.ProjectId, gcpCluster.Zone,
		deployer.ParentClusterId, deployment, log, true)
	if err != nil {
		return errors.New("Unable to create node pools: " + err.Error())
	}
	gcpCluster.NodePoolIds = nodePoolIds

	if err := populateNodeInfos(client, gcpProfile.ProjectId, gcpCluster.Zone, deployer.ParentClusterId,
		nodePoolIds, gcpCluster, deployment.ClusterDefinition, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	k8sClient, err := k8sUtil.RetryConnectKubernetes(deployer.KubeConfig)
	if err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	nodeNames := []string{}
	for _, nodeInfo := range gcpCluster.NodeInfos {
		nodeNames = append(nodeNames, nodeInfo.Instance.Name)
	}
	if err := k8sUtil.WaitUntilKubernetesNodeExists(k8sClient, nodeNames, time.Duration(2)*time.Minute, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	if err := insertFirewallIngressRules(client, gcpCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to insert firewall ingress rules: " + err.Error())
	}
	deployer.recordEndpoints(false)

	return nil
}

func (deployer *InClusterGCPDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset) error {
	log := deployer.GetLog().Logger
	namespace := deployer.getNamespace()
	if err := k8sUtil.CreateSecretsByNamespace(k8sClient, namespace, deployer.Deployment); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	log.Infof("Granting node-reader permission to namespace %s", namespace)
	if err := k8sUtil.GrantNodeReaderPermissionToNamespace(k8sClient, namespace, log); err != nil {
		// Assumption: if the action of grant failed, it wouldn't affect the whole deployment process which
		// is why we don't return an error here.
		log.Warningf("Unable to grant node-reader permission to namespace %s: %s", namespace, err.Error())
	}

	existingNamespaces, namespacesErr := k8sUtil.GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	userName := strings.ToLower(deployer.GCPCluster.GCPProfile.ServiceAccount)
	if err := k8sUtil.DeployServices(deployer.Config, k8sClient, deployer.Deployment,
		namespace, existingNamespaces, userName, log); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func (deployer *InClusterGCPDeployer) getNamespace() string {
	return strings.ToLower(deployer.GCPCluster.Name)
}

// UpdateDeployment start a deployment on GKE cluster is ready
func (deployer *InClusterGCPDeployer) UpdateDeployment(deployment *apis.Deployment) error {
	deployer.Deployment = deployment
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	namespace := deployer.getNamespace()
	if err := k8sUtil.DeleteK8S([]string{namespace}, deployer.KubeConfig, log); err != nil {
		return errors.New("Unable to delete k8s objects in update: " + err.Error())
	}

	k8sUtil.DeleteNodeReaderClusterRoleBindingToNamespace(k8sClient, namespace, log)

	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		return errors.New("Unable to deploy k8s objects in update: " + err.Error())
	}

	if err := updateFirewallIngressRules(client, gcpCluster, deployment, log); err != nil {
		return errors.New("Unable to update firewall ingress rules: " + err.Error())
	}
	deployer.recordEndpoints(true)

	return nil
}

func (deployer *InClusterGCPDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes: " + err.Error())
	}

	originalDeployment := deployer.Deployment
	deployer.Deployment = extensions
	if err := deployer.deployKubernetesObjects(k8sClient); err != nil {
		deployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}
	deployer.Deployment = newDeployment

	if err := updateFirewallIngressRules(client, gcpCluster, deployer.Deployment, log); err != nil {
		return errors.New("Unable to update firewall ingress rules: " + err.Error())
	}
	deployer.recordEndpoints(true)

	return nil
}

func deleteInClusterDeploymentOnFailure(deployer *InClusterGCPDeployer) {
	log := deployer.GetLog().Logger
	if deployer.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	deployer.DeleteDeployment()
}

// DeleteDeployment clean up the cluster from kubenetes.
func (deployer *InClusterGCPDeployer) DeleteDeployment() error {
	if len(deployer.GCPCluster.NodePoolIds) == 0 {
		return nil
	}

	deployment := deployer.Deployment
	kubeConfig := deployer.KubeConfig
	log := deployer.GetLog().Logger
	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to create k8s client: " + err.Error())
	}

	var errBool bool
	log.Infof("Deleting %s kubernetes deployment...", deployment.Name)
	namespace := deployer.getNamespace()
	if err := k8sUtil.DeleteK8S([]string{namespace}, kubeConfig, log); err != nil {
		errBool = true
		log.Warningf("Unable to deleting %s kubernetes deployment: %s", deployment.Name, err.Error())
	}

	k8sUtil.DeleteNodeReaderClusterRoleBindingToNamespace(k8sClient, namespace, log)

	if err := k8sClient.CoreV1().Namespaces().Delete(namespace, &metav1.DeleteOptions{}); err != nil {
		errBool = true
		log.Warningf("Unable to delete kubernetes namespace %s: %s", namespace, err.Error())
	}

	if err := deployer.deleteDeployment(); err != nil {
		errBool = true
		log.Warningf("Unable to deleting %s deployment: %s", deployment.Name, err.Error())
	}

	if errBool {
		return fmt.Errorf("Unable to delete %s deployment", deployment.Name)
	}

	return nil
}

func (deployer *InClusterGCPDeployer) deleteDeployment() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	projectId := gcpProfile.ProjectId
	zone := gcpCluster.Zone
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	if err := deleteNodePools(client, projectId, zone, deployer.ParentClusterId, gcpCluster.NodePoolIds); err != nil {
		return errors.New("Unable to delete node pools: %s" + err.Error())
	}

	firewallRuleNames := []string{fmt.Sprintf("gke-%s-http", gcpCluster.ClusterId)}
	if err := deleteFirewallRules(client, projectId, firewallRuleNames, log); err != nil {
		log.Warningf("Unable to delete firewall rules: " + err.Error())
	}

	log.Infof("Waiting until node pool to be delete completed...")
	if err := waitUntilNodePoolDeleteComplete(client, projectId, zone,
		deployer.ParentClusterId, gcpCluster.NodePoolIds, time.Duration(10)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until %s node pool to be delete completed: %s\n",
			deployer.ParentClusterId, err.Error())
	}

	return nil
}

// CheckClusterState check GCP in-cluster state is exist
func (deployer *InClusterGCPDeployer) CheckClusterState() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	projectId := gcpProfile.ProjectId
	zone := gcpCluster.Zone
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	inClusterDeploymentNames, err := findDeploymentNames(client, projectId, zone, deployer.ParentClusterId)
	if err != nil {
		return errors.New("Unable to find in-cluster deployment names: " + err.Error())
	}

	findInclusterDeployment := false
	for _, deploymentName := range inClusterDeploymentNames {
		if deploymentName == gcpCluster.ClusterId {
			findInclusterDeployment = true
			break
		}
	}
	if !findInclusterDeployment {
		return fmt.Errorf("Unable to %s deployment exist", gcpCluster.ClusterId)
	}

	return nil
}

func (deployer *InClusterGCPDeployer) ReloadClusterState(storeInfo interface{}) error {
	gcpStoreInfo := storeInfo.(*StoreInfo)
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	gcpCluster.ClusterId = gcpStoreInfo.ClusterId
	gcpCluster.Name = gcpStoreInfo.ClusterId
	projectId := gcpProfile.ProjectId
	zone := gcpCluster.Zone
	deployer.Deployment.Name = gcpCluster.ClusterId
	deploymentName := gcpCluster.ClusterId
	log := deployer.GetLog().Logger
	if err := deployer.CheckClusterState(); err != nil {
		return fmt.Errorf("Skipping reloading because unable to load %s cluster: %s", deploymentName, err.Error())
	}

	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	nodePoolIds, err := findNodePoolNames(client, projectId, zone, deployer.ParentClusterId, deploymentName)
	if err != nil {
		return errors.New("Unable to find in-cluster deployment names: " + err.Error())
	}

	if err := populateNodeInfos(client, projectId, zone, deployer.ParentClusterId,
		nodePoolIds, gcpCluster, deployer.Deployment.ClusterDefinition, log); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}
	deployer.recordEndpoints(false)

	return nil
}
