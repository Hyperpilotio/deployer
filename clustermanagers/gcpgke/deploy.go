package gcp

import (
	"errors"

	"github.com/hyperpilotio/go-utils/log"
	"github.com/spf13/viper"
	container "google.golang.org/api/container/v1"

	"github.com/hyperpilotio/deployer/apis"
	hpagcp "github.com/hyperpilotio/deployer/gcp"
)

// NewDeployer return the GCP of Deployer
func NewDeployer(config *viper.Viper, deployment *apis.Deployment) (*GCPDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	// TODO: api need pass param
	clusterVersion := "1.6.9"
	gcpProfile := &hpagcp.GCPProfile{
		UserId:    "alan",
		ProjectId: "test-179902",
		Scopes: []string{
			container.CloudPlatformScope,
		},
		ServiceAccountPath: "/home/alan/test-13274590fb39.json",
	}

	gcpCluster := hpagcp.NewGCPCluster(deployment.Region, deployment.Name, clusterVersion, gcpProfile)
	deployer := &GCPDeployer{
		Config:        config,
		GCPCluster:    gcpCluster,
		Deployment:    deployment,
		DeploymentLog: log,
	}

	return deployer, nil
}

// CreateDeployment start a deployment
func (gcpDeployer *GCPDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	gcpCluster := gcpDeployer.GCPCluster
	gcpProfile := gcpCluster.GCPProfile
	deployment := gcpDeployer.Deployment
	log := gcpDeployer.DeploymentLog.Logger

	client, err := hpagcp.CreateClient(gcpCluster.GCPProfile, gcpCluster.Zone)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	containerSrv, err := container.New(client)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	// test createClusterRequest param
	createClusterRequest := &container.CreateClusterRequest{
		Cluster: &container.Cluster{
			Name:              "cluster-1",
			Zone:              gcpCluster.Zone,
			Network:           "default",
			LoggingService:    "logging.googleapis.com",
			MonitoringService: "none",
			NodePools: []*container.NodePool{
				&container.NodePool{
					Name:             "default-pool",
					InitialNodeCount: int64(len(deployment.ClusterDefinition.Nodes)),
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
		return nil, errors.New("Unable to create deployment: " + err.Error())
	}
	log.Infof("%+v\n", resp)

	return nil, nil
}
