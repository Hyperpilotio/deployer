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
	projectId, err := getProjectId(gcpCluster.GCPProfile.AuthJSONFilePath)
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

func (deployer *GCPDeployer) GetKubeConfigPath() string {
	return deployer.KubeConfigPath
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

	log.Info("Updating kubernetes deployment")
	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := k8sUtil.DeleteK8S(k8sUtil.GetAllDeployedNamespaces(deployment), deployer.KubeConfig, log); err != nil {
		log.Warningf("Unable to delete k8s objects in update: " + err.Error())
	}

	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, log); err != nil {
		log.Warningf("Unable to deploy k8s objects in update: " + err.Error())
	}
	deployer.recordPublicEndpoints()

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
	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient,
		deployer.Deployment, deployer.GetLog().Logger); err != nil {
		deployer.Deployment = originalDeployment
		return errors.New("Unable to deploy k8s objects: " + err.Error())
	}

	deployer.Deployment = newDeployment
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (deployer *GCPDeployer) DeleteDeployment() error {
	deployment := deployer.Deployment
	kubeConfig := deployer.KubeConfig
	log := deployer.DeploymentLog.Logger

	// Deleting kubernetes deployment
	log.Infof("Deleting kubernetes deployment...")
	if err := k8sUtil.DeleteK8S(k8sUtil.GetAllDeployedNamespaces(deployment), kubeConfig, log); err != nil {
		log.Warningf("Unable to deleting kubernetes deployment: %s", err.Error())
	}

	if err := deployer.deleteDeployment(); err != nil {
		log.Warningf("Unable to deleting %s deployment: %s", deployment.Name, err.Error())
	}

	return nil
}

func (deployer *GCPDeployer) deleteDeployment() error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	log := deployer.DeploymentLog.Logger
	client, err := hpgcp.CreateClient(gcpCluster.GCPProfile, gcpCluster.Zone)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	containerSrv, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}
	_, err = containerSrv.Projects.Zones.Clusters.
		Delete(gcpProfile.ProjectId, gcpCluster.Zone, gcpCluster.ClusterId).
		Do()
	if err != nil {
		return errors.New("Unable to delete cluster: " + err.Error())
	}

	log.Infof("Waiting until cluster('%s') to be delete completed...", gcpCluster.ClusterId)
	if err := waitUntilClusterDeleteComplete(containerSrv, gcpProfile.ProjectId, gcpCluster.Zone,
		gcpCluster.ClusterId, time.Duration(10)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until %s cluster to be delete completed: %s\n",
			gcpCluster.ClusterId, err.Error())
	}
	log.Infof("Delete cluster('%s') ok...", gcpCluster.ClusterId)

	return nil
}

func deployCluster(deployer *GCPDeployer, uploadedFiles map[string]string) error {
	gcpCluster := deployer.GCPCluster
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	client, err := hpgcp.CreateClient(gcpCluster.GCPProfile, gcpCluster.Zone)
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

	if err := populateNodeInfos(client, gcpCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	if err := tagPublicKey(client, gcpCluster, log); err != nil {
		return errors.New("Unable to tag network Tags: " + err.Error())
	}

	if err := deployer.DownloadSSHKey(); err != nil {
		return errors.New("Unable to download ssh key: " + err.Error())
	}

	if err := deployer.uploadFiles(uploadedFiles); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to upload files to cluster: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(deployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := k8sUtil.DeployKubernetesObjects(deployer.Config, k8sClient, deployment, log); err != nil {
		deleteDeploymentOnFailure(deployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}
	deployer.recordPublicEndpoints()

	if err := tagNodeNetwork(client, gcpCluster, deployment, []string{"http-server"}, log); err != nil {
		return errors.New("Unable to tag network Tags: " + err.Error())
	}

	return nil
}

func deployKubernetes(client *http.Client, deployer *GCPDeployer) error {
	gcpCluster := deployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := deployer.Deployment
	log := deployer.GetLog().Logger
	containerSrv, err := container.New(client)
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
			InitialClusterVersion: gcpCluster.ClusterVersion,
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

	_, err = containerSrv.Projects.Zones.Clusters.
		Create(gcpProfile.ProjectId, gcpCluster.Zone, createClusterRequest).Do()
	if err != nil {
		return errors.New("Unable to create deployment: " + err.Error())
	}

	log.Info("Waiting until cluster is completed...")
	if err := waitUntilClusterCreateComplete(containerSrv, gcpProfile.ProjectId, gcpCluster.Zone,
		gcpCluster.ClusterId, time.Duration(10)*time.Minute, log); err != nil {
		return fmt.Errorf("Unable to wait until cluster complete: %s\n", err.Error())
	}
	log.Info("Kuberenete cluster completed")

	// Setting KubeConfig
	resp, _ := containerSrv.Projects.Zones.Clusters.
		List(gcpProfile.ProjectId, gcpCluster.Zone).
		Do()
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

	if err := deployer.DownloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}
	log.Infof("Downloaded kube config at %s", deployer.KubeConfigPath)

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

func (deployer *GCPDeployer) recordPublicEndpoints() {
	deployment := deployer.Deployment
	deployer.Services = map[string]ServiceMapping{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		for _, container := range task.Deployment.Spec.Template.Spec.Containers {
			hostPort := container.Ports[0].HostPort
			taskFamilyName := task.Family
			for _, nodeMapping := range deployment.NodeMapping {
				if nodeMapping.Task == taskFamilyName {
					nodeInfo, ok := deployer.GCPCluster.NodeInfos[nodeMapping.Id]
					if ok {
						serviceName := nodeInfo.Instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
						servicePort := strconv.FormatInt(int64(hostPort), 10)
						serviceMapping := ServiceMapping{
							NodeId:    nodeMapping.Id,
							NodeName:  nodeInfo.Instance.Name,
							PublicUrl: serviceName + ":" + servicePort,
						}
						deployer.Services[taskFamilyName] = serviceMapping
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
		file.Path = strings.Replace(file.Path, "$(SERVICE_ACCOUNT)", userName, -1)
		newDeployment.Files = append(newDeployment.Files, file)
	}

	for _, nodeInfo := range gcpCluster.NodeInfos {
		sshClient := common.NewSshClient(nodeInfo.PublicIp+":22", clientConfig, "")
		return common.UploadFiles(sshClient, newDeployment, uploadedFiles, log)
	}
	log.Info("Uploaded all files")

	return nil
}

func (deployer *GCPDeployer) DownloadKubeConfig() error {
	baseDir := deployer.GCPCluster.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	// TODO write kubeconfig yaml to kubeconfigFilePath

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
	// TODO
	return nil
}

// ReloadClusterState reloads kubernetes cluster state
func (deployer *GCPDeployer) ReloadClusterState(storeInfo interface{}) error {
	// TODO
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
	return nil
}

func (deployer *GCPDeployer) NewStoreInfo() interface{} {
	return nil
}
