package gcpgke

import (
	"errors"
	"fmt"
	"io/ioutil"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
	container "google.golang.org/api/container/v1"

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

	serviceAccount, err := findServiceAccount(client, projectId, deployment.Region, parentClusterId, log.Logger)
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
			Services:      make(map[string]k8sUtil.ServiceMapping),
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

	if err := deployKubernetes(client, gcpCluster, deployment, log); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := deployer.setKubeConfig(); err != nil {
		return errors.New("Unable to set GCP deployer kubeconfig: " + err.Error())
	}

	nodePoolName := []string{"default-pool"}
	if err := populateNodeInfos(client, gcpProfile.ProjectId, gcpCluster.Zone, gcpCluster.ClusterId,
		nodePoolName, gcpCluster, deployment.ClusterDefinition, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := deployer.DownloadKubeConfig(); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}
	log.Infof("Downloaded kube config at %s", deployer.KubeConfigPath)

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	nodeNames := []string{}
	for _, nodeInfo := range gcpCluster.NodeInfos {
		nodeNames = append(nodeNames, nodeInfo.Instance.Name)
	}
	if err := k8sUtil.WaitUntilKubernetesNodeExists(k8sClient, nodeNames, time.Duration(3)*time.Minute, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	userName := strings.ToLower(gcpCluster.GCPProfile.ServiceAccount)
	serviceMappings, err := k8sUtil.DeployKubernetesObjects(deployer.Config, deployer.KubeConfig, deployment, userName, log)
	if err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	if err := insertFirewallIngressRules(client, gcpCluster, deployment, log); err != nil {
		deleteInClusterDeploymentOnFailure(deployer)
		return errors.New("Unable to insert firewall ingress rules: " + err.Error())
	}
	deployer.Services = serviceMappings
	deployer.recordEndpoints(false)

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
	if err := deployer.deleteDeployment(); err != nil {
		return fmt.Errorf("Unable to deleting %s deployment: %s",
			deployer.Deployment.Name, err.Error())
	}

	return nil
}

func (deployer *InClusterGCPDeployer) deleteDeployment() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	projectId := gcpProfile.ProjectId
	zone := gcpCluster.Zone
	clusterId := gcpCluster.ClusterId
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	_, err = containerSvc.Projects.Zones.Clusters.
		Delete(projectId, zone, clusterId).
		Do()
	if err != nil {
		return errors.New("Unable to delete cluster: " + err.Error())
	}

	log.Infof("Waiting until cluster to be delete completed...")
	if err := waitUntilClusterDeleteComplete(containerSvc, projectId, zone,
		clusterId, time.Duration(10)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until %s cluster to be delete completed: %s\n",
			clusterId, err.Error())
	}

	if err := deleteFirewallRules(client, projectId, zone, log); err != nil {
		log.Warningf("Unable to delete firewall rules: " + err.Error())
	}

	return nil
}
