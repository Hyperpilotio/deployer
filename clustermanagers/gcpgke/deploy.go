package gcpgke

import (
	"errors"
	"net/http"

	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
	container "google.golang.org/api/container/v1"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/job"
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

	deployer := &GCPDeployer{
		Config:        config,
		GCPCluster:    cluster.(*hpgcp.GCPCluster),
		Deployment:    deployment,
		DeploymentLog: log,
	}

	return deployer, nil
}

// CreateDeployment start a deployment
func (gcpDeployer *GCPDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	if err := deployCluster(gcpDeployer, uploadedFiles); err != nil {
		return nil, errors.New("Unable to deploy kubernetes: " + err.Error())
	}

	return nil, nil
}

func (gcpDeployer *GCPDeployer) DeployExtensions(extensions *apis.Deployment, mergedDeployment *apis.Deployment) error {
	return nil
}

func (gcpDeployer *GCPDeployer) UpdateDeployment(updateDeployment *apis.Deployment) error {
	return nil
}

func (gcpDeployer *GCPDeployer) DeleteDeployment() error {
	return nil
}

func (gcpDeployer *GCPDeployer) ReloadClusterState(storeInfo interface{}) error {
	return nil
}

func (gcpDeployer *GCPDeployer) GetStoreInfo() interface{} {
	return nil
}

func (gcpDeployer *GCPDeployer) NewStoreInfo() interface{} {
	return nil
}

func (gcpDeployer *GCPDeployer) GetCluster() clusters.Cluster {
	return gcpDeployer.GCPCluster
}

func (gcpDeployer *GCPDeployer) GetLog() *log.FileLog {
	return gcpDeployer.DeploymentLog
}

func (gcpDeployer *GCPDeployer) GetScheduler() *job.Scheduler {
	return gcpDeployer.Scheduler
}

func (gcpDeployer *GCPDeployer) SetScheduler(sheduler *job.Scheduler) {
	gcpDeployer.Scheduler = sheduler
}

func (gcpDeployer *GCPDeployer) GetServiceUrl(serviceName string) (string, error) {
	return "", nil
}

func (gcpDeployer *GCPDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	return nil, nil
}

func (gcpDeployer *GCPDeployer) GetServiceMappings() (map[string]interface{}, error) {
	return nil, nil
}

func deployCluster(gcpDeployer *GCPDeployer, uploadedFiles map[string]string) error {
	gcpCluster := gcpDeployer.GCPCluster
	// gcpProfile := gcpCluster.GCPProfile
	// deployment := gcpDeployer.Deployment
	// log := gcpDeployer.DeploymentLog.Logger
	client, err := hpgcp.CreateClient(gcpCluster.GCPProfile, gcpCluster.Zone)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	if err := deployKubernetes(client, gcpDeployer); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	// if err := populateNodeInfos(ec2Svc, awsCluster); err != nil {
	// 	return errors.New("Unable to populate node infos: " + err.Error())
	// }

	return nil
}

func deployKubernetes(client *http.Client, gcpDeployer *GCPDeployer) error {
	gcpCluster := gcpDeployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := gcpDeployer.Deployment
	log := gcpDeployer.DeploymentLog.Logger
	containerSrv, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	initialNodeCount := 3
	deployNodeCount := len(deployment.ClusterDefinition.Nodes)
	if deployNodeCount > initialNodeCount {
		initialNodeCount = deployNodeCount
	}

	// test createClusterRequest param
	createClusterRequest := &container.CreateClusterRequest{
		Cluster: &container.Cluster{
			Name:              deployment.Name,
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

	resp, err := containerSrv.Projects.Zones.Clusters.
		Create(gcpProfile.ProjectId, gcpCluster.Zone, createClusterRequest).Do()
	if err != nil {
		return errors.New("Unable to create deployment: " + err.Error())
	}
	log.Infof("%+v\n", resp)

	// TODO WaitUntilStackCreateComplete
	log.Info("Waiting until cluster is completed...")

	// if err := cfSvc.WaitUntilStackCreateComplete(describeStacksInput); err != nil {
	// 	return errors.New("Unable to wait until stack complete: " + err.Error())
	// }

	// TODO DownloadKubeConfig
	// if err := k8sDeployer.DownloadKubeConfig(); err != nil {
	// 	return errors.New("Unable to download kubeconfig: " + err.Error())
	// }
	// log.Infof("Downloaded kube config at %s", k8sDeployer.KubeConfigPath)

	return nil
}
