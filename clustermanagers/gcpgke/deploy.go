package gcpgke

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	container "google.golang.org/api/container/v1"

	"github.com/hyperpilotio/deployer/apis"
	k8sUtil "github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/clusters"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/common"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewDeployer return the GCP of Deployer
func NewDeployer(
	config *viper.Viper,
	cluster clusters.Cluster,
	deployment *apis.Deployment) (*GCPDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	gcpCluster := cluster.(*hpgcp.GCPCluster)
	if gcpCluster.GCPProfile == nil {
		return nil, errors.New("Unable to find GCP Profile from user")
	}

	projectId, err := gcpCluster.GCPProfile.GetProjectId()
	if err != nil {
		return nil, errors.New("Unable to find projectId: " + err.Error())
	}
	gcpCluster.GCPProfile.ProjectId = projectId

	if gcpCluster.GCPProfile.ServiceAccount == "" {
		return nil, errors.New("Unable to find serviceAccount: " + err.Error())
	}

	deployer := &GCPDeployer{
		Config:        config,
		GCPCluster:    gcpCluster,
		Deployment:    deployment,
		DeploymentLog: log,
		Services:      map[string]k8sUtil.ServiceMapping{},
	}

	return deployer, nil
}

func (deployer *GCPDeployer) GetLog() *log.FileLog {
	return deployer.DeploymentLog
}

func (deployer *GCPDeployer) GetScheduler() *job.Scheduler {
	return deployer.Scheduler
}

func (deployer *GCPDeployer) SetScheduler(sheduler *job.Scheduler) {
	deployer.Scheduler = sheduler
}

func (deployer *GCPDeployer) GetKubeConfigPath() (string, error) {
	return deployer.KubeConfigPath, nil
}

// CreateDeployment start a deployment
func (deployer *GCPDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	if err := deployCluster(deployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	response := &CreateDeploymentResponse{
		Name:      deployer.Deployment.Name,
		ClusterId: deployer.GCPCluster.ClusterId,
		Services:  deployer.Services,
	}

	return response, nil
}

// UpdateDeployment start a deployment on GCP is ready
func (deployer *GCPDeployer) UpdateDeployment(deployment *apis.Deployment) error {
	deployer.Deployment = deployment
	log := deployer.GetLog().Logger
	serviceMappings := map[string]k8sUtil.ServiceMapping{}

	log.Info("Updating kubernetes deployment")
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := k8sUtil.DeleteK8S(k8sUtil.GetAllDeployedNamespaces(deployment), deployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	gcpCluster := deployer.GCPCluster
	userName := strings.ToLower(gcpCluster.GCPProfile.ServiceAccount)
	serviceMappings, err = k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, userName, log)
	if err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}
	deployer.Services = serviceMappings
	deployer.recordEndpoints(true)

	return nil
}

func (deployer *GCPDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes: " + err.Error())
	}

	originalDeployment := deployer.Deployment
	deployer.Deployment = extensions
	gcpCluster := deployer.GCPCluster
	userName := strings.ToLower(gcpCluster.GCPProfile.ServiceAccount)
	serviceMappings, err := k8sUtil.DeployKubernetesObjects(
		deployer.Config,
		k8sClient,
		deployer.Deployment,
		userName,
		deployer.GetLog().Logger)
	if err != nil {
		deployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}

	deployer.Services = serviceMappings
	deployer.Deployment = newDeployment
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (deployer *GCPDeployer) DeleteDeployment() error {
	if err := deployer.deleteDeployment(); err != nil {
		return fmt.Errorf("Unable to deleting %s deployment: %s",
			deployer.Deployment.Name, err.Error())
	}

	return nil
}

func (deployer *GCPDeployer) deleteDeployment() error {
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
	if err := waitUntilClusterStatusRunning(containerSvc, projectId, zone,
		clusterId, time.Duration(5)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until cluster complete: %s\n", err.Error())
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

	if err := deletePublicKey(client, gcpCluster, log); err != nil {
		log.Warningf("Unable to delete firewall rules: " + err.Error())
	}

	return nil
}

func deployCluster(deployer *GCPDeployer, uploadedFiles map[string]string) error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpProfile)
	serviceMappings := map[string]k8sUtil.ServiceMapping{}
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	if keyOutput, err := hpgcp.CreateKeypair(gcpCluster.ClusterId); err != nil {
		return errors.New("Unable to create key pair: " + err.Error())
	} else {
		gcpCluster.KeyPair = keyOutput
	}

	if err := deployKubernetes(client, deployer); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	nodePoolName := []string{"default-pool"}
	if err := populateNodeInfos(client, gcpProfile.ProjectId, gcpCluster.Zone, gcpCluster.ClusterId,
		nodePoolName, gcpCluster, deployment.ClusterDefinition, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := tagPublicKey(client, gcpCluster, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to tag publicKey to node instance metadata: " + err.Error())
	}

	if err := deployer.DownloadSSHKey(); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to download ssh key: " + err.Error())
	}

	if err := deployer.DownloadKubeConfig(); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}
	log.Infof("Downloaded kube config at %s", deployer.KubeConfigPath)

	if err := deployer.uploadFiles(uploadedFiles); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during create: " + err.Error())
	}

	nodeNames := []string{}
	for _, nodeInfo := range gcpCluster.NodeInfos {
		nodeNames = append(nodeNames, nodeInfo.Instance.Name)
	}
	if err := k8sUtil.WaitUntilKubernetesNodeExists(k8sClient, nodeNames, time.Duration(3)*time.Minute, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable wait for kubernetes nodes to be exist: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	userName := strings.ToLower(gcpCluster.GCPProfile.ServiceAccount)
	serviceMappings, err = k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, userName, log)
	if err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

	if err := insertFirewallIngressRules(client, gcpCluster, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to insert firewall ingress rules: " + err.Error())
	}
	deployer.Services = serviceMappings
	deployer.recordEndpoints(false)

	return nil
}

func deployKubernetes(client *http.Client, deployer *GCPDeployer) error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	initialNodeCount := 3
	deployNodeCount := len(deployment.ClusterDefinition.Nodes)
	if deployNodeCount > initialNodeCount {
		initialNodeCount = deployNodeCount
	}

	createClusterRequest := &container.CreateClusterRequest{
		Cluster: &container.Cluster{
			Name:              gcpCluster.ClusterId,
			Zone:              gcpCluster.Zone,
			Network:           "default",
			LoggingService:    "logging.googleapis.com",
			MonitoringService: "none",
			NodePools: []*container.NodePool{
				&container.NodePool{
					Name:             "default-pool",
					InitialNodeCount: int64(initialNodeCount),
					Config: &container.NodeConfig{
						MachineType: deployment.ClusterDefinition.Nodes[0].InstanceType,
						ImageType:   "COS",
						DiskSizeGb:  int64(100),
						Preemptible: false,
						OauthScopes: []string{
							"https://www.googleapis.com/auth/compute",
							"https://www.googleapis.com/auth/devstorage.read_only",
							"https://www.googleapis.com/auth/logging.write",
							"https://www.googleapis.com/auth/monitoring.write",
							"https://www.googleapis.com/auth/servicecontrol",
							"https://www.googleapis.com/auth/service.management.readonly",
							"https://www.googleapis.com/auth/trace.append",
						},
						Metadata: map[string]string{
							"serviceAccount": gcpProfile.ServiceAccount,
						},
					},
					Autoscaling: &container.NodePoolAutoscaling{
						Enabled: false,
					},
					Management: &container.NodeManagement{
						AutoUpgrade:    false,
						AutoRepair:     false,
						UpgradeOptions: &container.AutoUpgradeOptions{},
					},
				},
			},
			MasterAuth: &container.MasterAuth{
				Username: "admin",
				ClientCertificateConfig: &container.ClientCertificateConfig{
					IssueClientCertificate: true,
				},
			},
			Subnetwork: "default",
			LegacyAbac: &container.LegacyAbac{
				Enabled: true,
			},
			MasterAuthorizedNetworksConfig: &container.MasterAuthorizedNetworksConfig{
				Enabled:    false,
				CidrBlocks: []*container.CidrBlock{},
			},
		},
	}

	if gcpCluster.ClusterVersion != "" {
		createClusterRequest.Cluster.InitialClusterVersion = gcpCluster.ClusterVersion
	}

	_, err = containerSvc.Projects.Zones.Clusters.
		Create(gcpProfile.ProjectId, gcpCluster.Zone, createClusterRequest).Do()
	if err != nil {
		return errors.New("Unable to create deployment: " + err.Error())
	}

	log.Info("Waiting until cluster is completed...")
	if err := waitUntilClusterStatusRunning(containerSvc, gcpProfile.ProjectId, gcpCluster.Zone,
		gcpCluster.ClusterId, time.Duration(10)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until cluster complete: %s\n", err.Error())
	}
	log.Info("Kuberenete cluster completed")

	// Setting KubeConfig
	if err := deployer.setKubeConfig(); err != nil {
		return errors.New("Unable to set GCP deployer kubeconfig: " + err.Error())
	}

	return nil
}

func deleteDeploymentOnFailure(deployer *GCPDeployer) {
	log := deployer.DeploymentLog.Logger
	if deployer.Deployment.KubernetesDeployment.SkipDeleteOnFailure {
		log.Warning("Skipping delete deployment on failure")
		return
	}

	deployer.DeleteDeployment()
}

func (deployer *GCPDeployer) recordEndpoints(reset bool) {
	if reset {
		deployer.Services = map[string]k8sUtil.ServiceMapping{}
	}
	deployment := deployer.Deployment
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.PortTypes == nil || len(task.PortTypes) == 0 {
			continue
		}
		ports := task.GetPorts()
		for i, portType := range task.PortTypes {
			hostPort := ports[i].HostPort
			taskFamilyName := task.Family
			for _, nodeMapping := range deployment.NodeMapping {
				if nodeMapping.Task == taskFamilyName {
					nodeInfo, ok := deployer.GCPCluster.NodeInfos[nodeMapping.Id]
					if ok {
						serviceName := nodeInfo.Instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
						servicePort := strconv.FormatInt(int64(hostPort), 10)
						serviceMapping := k8sUtil.ServiceMapping{}
						url := serviceName + ":" + servicePort
						if portType == publicPortType {
							serviceMapping.PublicUrl = url
						}
						serviceMapping.NodeName = nodeInfo.Instance.Name

						assignedTaskName := taskFamilyName
						for taskName, serviceMapping := range deployer.Services {
							if strings.HasPrefix(taskName, taskFamilyName) && serviceMapping.NodeId == nodeMapping.Id {
								assignedTaskName = taskName
								break
							}
						}
						deployer.Services[assignedTaskName] = serviceMapping
					}
				}
			}

		}
	}
}

func (deployer *GCPDeployer) uploadFiles(uploadedFiles map[string]string) error {
	gcpCluster := deployer.GCPCluster
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	if len(deployment.Files) == 0 {
		return nil
	}

	userName := strings.ToLower(gcpCluster.GCPProfile.ServiceAccount)
	clientConfig, clientConfigErr := gcpCluster.SshConfig(userName)
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	newDeployment := &apis.Deployment{UserId: deployment.UserId}
	for _, file := range deployment.Files {
		if strings.HasPrefix(file.Path, "~/") {
			file.Path = strings.Replace(file.Path, "~/", "/home/"+userName+"/", 1)
		}
		newDeployment.Files = append(newDeployment.Files, file)
	}

	for _, nodeInfo := range gcpCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PublicIp+":22", clientConfig, "")
		if err := common.UploadFiles(sshClient, newDeployment, uploadedFiles, log); err != nil {
			return fmt.Errorf("Unable to upload file to node %s: %s", nodeInfo.PublicIp, err.Error())
		}
	}

	log.Info("Uploaded all files")
	return nil
}

func (deployer *GCPDeployer) DownloadKubeConfig() error {
	gcpCluster := deployer.GCPCluster
	projectId := gcpCluster.GCPProfile.ProjectId
	zone := gcpCluster.Zone
	clusterId := gcpCluster.ClusterId
	baseDir := gcpCluster.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	kubeconfigYaml := kubeconfigYamlTemplate
	kubeconfigYaml = strings.Replace(kubeconfigYaml, "$PROJECT_ID", projectId, -1)
	kubeconfigYaml = strings.Replace(kubeconfigYaml, "$ZONE", zone, -1)
	kubeconfigYaml = strings.Replace(kubeconfigYaml, "$CLUSTER_ID", clusterId, -1)
	kubeconfigYaml = strings.Replace(kubeconfigYaml, "$PASSWORD", deployer.KubeConfig.Password, -1)
	kubeconfigYaml = strings.Replace(kubeconfigYaml, "$USERNAME", deployer.KubeConfig.Username, -1)

	replaceKeyWords := []string{"CA_CERT", "KUBERNETES_MASTER_NAME"}
	for _, item := range deployer.GCPCluster.NodeInfos[1].Instance.Metadata.Items {
		if item.Key == "kube-env" {
			values := strings.Split(*item.Value, "\n")
			for _, keyWord := range replaceKeyWords {
				for _, val := range values {
					if strings.HasPrefix(val, keyWord+":") {
						data := val
						data = strings.Replace(data, keyWord+":", "", -1)
						data = strings.Replace(data, " ", "", -1)
						kubeconfigYaml = strings.Replace(kubeconfigYaml, "$"+keyWord, data, -1)
						break
					}
				}
			}
		}
	}
	if err := ioutil.WriteFile(kubeconfigFilePath, []byte(kubeconfigYaml), 0666); err != nil {
		return fmt.Errorf("Unable to create %s kubeconfig file: %s",
			gcpCluster.Name, err.Error())
	}

	deployer.KubeConfigPath = kubeconfigFilePath
	return nil
}

func (deployer *GCPDeployer) DownloadSSHKey() error {
	baseDir := deployer.GCPCluster.Name + "_sshkey"
	basePath := "/tmp/" + baseDir
	sshKeyFilePath := basePath + "/" + deployer.GCPCluster.KeyName() + ".pem"
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}
	os.Remove(sshKeyFilePath)

	privateKey := strings.Replace(deployer.GCPCluster.KeyPair.Pem, "\\n", "\n", -1)
	if err := ioutil.WriteFile(sshKeyFilePath, []byte(privateKey), 0400); err != nil {
		return fmt.Errorf("Unable to create %s sshKey file: %s",
			deployer.GCPCluster.KeyPair.KeyName, err.Error())
	}

	return nil
}

func (deployer *GCPDeployer) GetCluster() clusters.Cluster {
	return deployer.GCPCluster
}

// CheckClusterState check kubernetes cluster state is exist
func (deployer *GCPDeployer) CheckClusterState() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	_, err = containerSvc.Projects.Zones.Clusters.NodePools.
		List(gcpProfile.ProjectId, gcpCluster.Zone, gcpCluster.ClusterId).
		Do()
	if err != nil && strings.Contains(err.Error(), "was not found") {
		return fmt.Errorf("Unable to find %s cluster to be reload", gcpCluster.ClusterId)
	}

	return nil
}

func (deployer *GCPDeployer) setKubeConfig() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	resp, err := containerSvc.Projects.Zones.Clusters.
		List(gcpProfile.ProjectId, gcpCluster.Zone).
		Do()
	if err != nil {
		return errors.New("Unable to list google cloud platform cluster: " + err.Error())
	}

	for _, cluster := range resp.Clusters {
		if cluster.Name == gcpCluster.ClusterId {
			cert, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClientCertificate)
			if err != nil {
				return errors.New("Unable to decode clientCertificate: " + err.Error())
			}
			key, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClientKey)
			if err != nil {
				return errors.New("Unable to decode clientKey: " + err.Error())
			}
			ca, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClusterCaCertificate)
			if err != nil {
				return errors.New("Unable to decode clusterCaCertificate: " + err.Error())
			}
			config := &rest.Config{
				Host:            cluster.Endpoint,
				TLSClientConfig: rest.TLSClientConfig{CertData: cert, KeyData: key, CAData: ca},
				Username:        cluster.MasterAuth.Username,
				Password:        cluster.MasterAuth.Password,
			}
			deployer.KubeConfig = config
		}
	}

	return nil
}

// ReloadClusterState reloads kubernetes cluster state
func (deployer *GCPDeployer) ReloadClusterState(storeInfo interface{}) error {
	gcpStoreInfo := storeInfo.(*StoreInfo)
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	gcpCluster.ClusterId = gcpStoreInfo.ClusterId
	gcpCluster.Name = gcpStoreInfo.ClusterId
	deployer.Deployment.Name = gcpCluster.ClusterId
	deploymentName := gcpCluster.ClusterId

	// Need to reset log name with deployment name
	deployer.GetLog().LogFile.Close()
	log, err := log.NewLogger(deployer.Config.GetString("filesPath"), deploymentName)
	if err != nil {
		return errors.New("Error creating deployment logger: " + err.Error())
	}
	deployer.DeploymentLog = log

	if err := deployer.CheckClusterState(); err != nil {
		return fmt.Errorf("Skipping reloading because unable to load %s cluster: %s", deploymentName, err.Error())
	}

	if err := deployer.setKubeConfig(); err != nil {
		return errors.New("Unable to set GCP deployer kubeconfig: " + err.Error())
	}

	client, err := hpgcp.CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	nodePoolName := []string{"default-pool"}
	if err := populateNodeInfos(client, gcpProfile.ProjectId, gcpCluster.Zone, gcpCluster.ClusterId,
		nodePoolName, gcpCluster, deployer.Deployment.ClusterDefinition, log.Logger); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}
	deployer.recordEndpoints(false)

	log.Logger.Infof("Reloading kube config for %s...", deployer.GCPCluster.Name)
	if err := deployer.DownloadKubeConfig(); err != nil {
		return fmt.Errorf("Unable to download %s kubeconfig: %s", deploymentName, err.Error())
	}
	log.Logger.Infof("Reloaded %s kube config at %s", deployer.GCPCluster.Name, deployer.KubeConfigPath)

	return nil
}

func (deployer *GCPDeployer) GetServiceMappings() (map[string]interface{}, error) {
	nodeNameInfos := map[string]string{}
	if len(deployer.GCPCluster.NodeInfos) > 0 {
		for id, nodeInfo := range deployer.GCPCluster.NodeInfos {
			nodeNameInfos[strconv.Itoa(id)] = nodeInfo.Instance.Name
		}
	} else {
		k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
		if err != nil {
			return nil, errors.New("Unable to connect to Kubernetes: " + err.Error())
		}

		nodes, nodeError := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if nodeError != nil {
			return nil, fmt.Errorf("Unable to list nodes: %s", nodeError.Error())
		}

		for _, node := range nodes.Items {
			nodeNameInfos[node.Labels["hyperpilot/node-id"]] = node.Name
		}
	}

	serviceMappings := make(map[string]interface{})
	for serviceName, serviceMapping := range deployer.Services {
		if serviceMapping.NodeId == 0 {
			serviceNodeId, err := k8sUtil.FindNodeIdFromServiceName(deployer.Deployment, serviceName)
			if err != nil {
				return nil, fmt.Errorf("Unable to find %s node id: %s", serviceName, err.Error())
			}
			serviceMapping.NodeId = serviceNodeId
		}
		serviceMapping.NodeName = nodeNameInfos[strconv.Itoa(serviceMapping.NodeId)]
		serviceMappings[serviceName] = serviceMapping
	}

	return serviceMappings, nil
}

// GetServiceAddress return ServiceAddress object
func (deployer *GCPDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	for _, task := range deployer.Deployment.KubernetesDeployment.Kubernetes {
		if task.Family == serviceName {
			for _, container := range task.Deployment.Spec.Template.Spec.Containers {
				for _, nodeMapping := range deployer.Deployment.NodeMapping {
					if nodeMapping.Task == serviceName {
						nodeInfo, ok := deployer.GCPCluster.NodeInfos[nodeMapping.Id]
						if ok {
							return &apis.ServiceAddress{
								Host: nodeInfo.Instance.Name,
								Port: container.Ports[0].HostPort,
							}, nil
						}
					}
				}
			}
		}
	}

	return nil, errors.New("Service not found in endpoints")
}

func (deployer *GCPDeployer) GetServiceUrl(serviceName string) (string, error) {
	if info, ok := deployer.Services[serviceName]; ok {
		return info.PublicUrl, nil
	}

	for _, task := range deployer.Deployment.KubernetesDeployment.Kubernetes {
		if task.Family == serviceName {
			for _, container := range task.Deployment.Spec.Template.Spec.Containers {
				hostPort := container.Ports[0].HostPort
				for _, nodeMapping := range deployer.Deployment.NodeMapping {
					if nodeMapping.Task == serviceName {
						nodeInfo, ok := deployer.GCPCluster.NodeInfos[nodeMapping.Id]
						if ok {
							serviceName := nodeInfo.Instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
							servicePort := strconv.FormatInt(int64(hostPort), 10)
							return serviceName + ":" + servicePort, nil
						}
					}
				}
			}
		}
	}

	return "", errors.New("Service not found in endpoints")
}

func (deployer *GCPDeployer) GetStoreInfo() interface{} {
	return &StoreInfo{
		ClusterId: deployer.GCPCluster.ClusterId,
	}
}

func (deployer *GCPDeployer) NewStoreInfo() interface{} {
	return &StoreInfo{}
}
