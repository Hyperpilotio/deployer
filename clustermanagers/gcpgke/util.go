package gcpgke

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	k8sUtil "github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/go-utils/funcs"

	logging "github.com/op/go-logging"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"

	k8s "k8s.io/client-go/kubernetes"
)

var publicPortType = 1

func createUniqueNodePoolName() string {
	timeSeq := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("default-pool-%s", timeSeq)
}

func createNodePools(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	deployment *apis.Deployment,
	log *logging.Logger,
	custEachNodeInstanceType bool) ([]string, error) {
	containerSvc, err := container.New(client)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	nodePoolIds := []string{}
	if custEachNodeInstanceType {
		// TODO group by instance type to create node pool
		for _, node := range deployment.ClusterDefinition.Nodes {
			nodePoolName := createUniqueNodePoolName()
			nodePoolRequest := NewNodePoolRequest(nodePoolName, node.InstanceType, 1)
			_, err := containerSvc.Projects.Zones.Clusters.NodePools.
				Create(projectId, zone, clusterId, nodePoolRequest).
				Do()
			if err != nil {
				return nil, fmt.Errorf("Unable to create %s node pool: %s", nodePoolName, err.Error())
			}
			nodePoolIds = append(nodePoolIds, nodePoolName)
		}
	} else {
		nodePoolSize := len(deployment.ClusterDefinition.Nodes)
		instanceType := deployment.ClusterDefinition.Nodes[0].InstanceType
		nodePoolName := createUniqueNodePoolName()
		nodePoolRequest := NewNodePoolRequest(nodePoolName, instanceType, nodePoolSize)
		_, err := containerSvc.Projects.Zones.Clusters.NodePools.
			Create(projectId, zone, clusterId, nodePoolRequest).
			Do()
		if err != nil {
			return nil, fmt.Errorf("Unable to create %s node pool: %s", nodePoolName, err.Error())
		}
		nodePoolIds = append(nodePoolIds, nodePoolName)
	}

	log.Info("Waiting until node pool is completed...")
	if err := waitUntilNodePoolCreateComplete(containerSvc, projectId, zone,
		clusterId, nodePoolIds, time.Duration(10)*time.Minute, log); err != nil {
		return nil, fmt.Errorf("Unable to wait until cluster complete: %s\n", err.Error())
	}

	return nodePoolIds, nil
}

func NewNodePoolRequest(
	nodePoolName string,
	machineType string,
	nodePoolSize int) *container.CreateNodePoolRequest {
	return &container.CreateNodePoolRequest{
		NodePool: &container.NodePool{
			Name:             nodePoolName,
			InitialNodeCount: int64(nodePoolSize),
			Config: &container.NodeConfig{
				MachineType: machineType,
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
	}
}

func populateNodeInfos(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	nodePoolIds []string,
	gcpCluster *hpgcp.GCPCluster) error {
	instanceGroupNames := []string{}
	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}
	for _, nodePoolId := range nodePoolIds {
		resp, err := containerSvc.Projects.Zones.Clusters.NodePools.
			Get(projectId, zone, clusterId, nodePoolId).
			Do()
		if err != nil {
			return fmt.Errorf("Unable to get %s node pool: ", nodePoolId, err.Error())
		}
		for _, instanceGroupUrl := range resp.InstanceGroupUrls {
			urls := strings.Split(instanceGroupUrl, "/")
			instanceGroupNames = append(instanceGroupNames, urls[len(urls)-1])
		}
	}

	instanceNames := []string{}
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}
	for _, instanceGroupName := range instanceGroupNames {
		resp, err := computeSvc.InstanceGroups.ListInstances(projectId, zone, instanceGroupName,
			&compute.InstanceGroupsListInstancesRequest{
				InstanceState: "RUNNING",
			}).Do()
		if err != nil {
			return errors.New("Unable to list instance: " + err.Error())
		}
		for _, item := range resp.Items {
			urls := strings.Split(item.Instance, "/")
			instanceNames = append(instanceNames, urls[len(urls)-1])
		}
	}

	clusterInstances := []*compute.Instance{}
	for _, instanceName := range instanceNames {
		instance, err := computeSvc.Instances.Get(projectId, zone, instanceName).Do()
		if err != nil {
			return fmt.Errorf("Unable to get %s instances: %s", instanceName, err.Error())
		}
		clusterInstances = append(clusterInstances, instance)
	}

	i := 1
	for _, instance := range clusterInstances {
		nodeInfo := &hpgcp.NodeInfo{
			Instance:  instance,
			PublicIp:  instance.NetworkInterfaces[0].AccessConfigs[0].NatIP,
			PrivateIp: instance.NetworkInterfaces[0].NetworkIP,
		}
		gcpCluster.NodeInfos[i] = nodeInfo
		i += 1
	}

	return nil
}

func tagKubeNodes(
	k8sClient *k8s.Clientset,
	gcpCluster *hpgcp.GCPCluster,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	nodeInfos := map[string]int{}
	for _, mapping := range deployment.NodeMapping {
		instanceName := gcpCluster.NodeInfos[mapping.Id].Instance.Name
		nodeInfos[instanceName] = mapping.Id
	}

	return k8sUtil.TagKubeNodes(k8sClient, deployment.Name, nodeInfos, log)
}

func tagFirewallIngressRules(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	// TODO need to allowed ports by node deploy task
	allowedPorts := []string{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.PortTypes == nil || len(task.PortTypes) == 0 {
			continue
		}

		ports := task.GetPorts()
		for i, portType := range task.PortTypes {
			if portType != publicPortType {
				log.Infof("Skipping creating public endpoint for service %s as it's marked as private", task.Family)
				continue
			}

			hostPort := strconv.Itoa(int(ports[i].HostPort))

			allowedPortExist := false
			for _, allowedPort := range allowedPorts {
				if allowedPort == hostPort {
					allowedPortExist = true
					break
				}
			}

			if !allowedPortExist {
				allowedPorts = append(allowedPorts, hostPort)
			}
		}
	}

	targetTagName := fmt.Sprintf("gke-%s-http-server", gcpCluster.ClusterId)
	_, err = computeSvc.Firewalls.Insert(projectId, &compute.Firewall{
		Allowed: []*compute.FirewallAllowed{
			&compute.FirewallAllowed{
				IPProtocol: "tcp",
				Ports:      allowedPorts,
			},
		},
		Description: "INGRESS",
		Name:        fmt.Sprintf("gke-%s-http", gcpCluster.ClusterId),
		Priority:    int64(1000),
		TargetTags:  []string{targetTagName},
	}).Do()
	if err != nil {
		return errors.New("Unable to create firewall ingress rules: " + err.Error())
	}

	var errBool bool
	for _, nodeInfo := range gcpCluster.NodeInfos {
		instanceName := nodeInfo.Instance.Name
		newTags := *nodeInfo.Instance.Tags
		newTags.Items = append(newTags.Items, targetTagName)
		_, err := computeSvc.Instances.
			SetTags(projectId, gcpCluster.Zone, instanceName, &newTags).
			Do()
		if err != nil {
			errBool = true
		}
	}
	if errBool {
		return errors.New("Unable to tag network for all node")
	}

	return nil
}

func deleteFirewallRules(client *http.Client, projectId string, firewallName string) error {
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	_, err = computeSvc.Firewalls.Delete(projectId, firewallName).Do()
	if err != nil {
		return errors.New("Unable to delete firewall rules: " + err.Error())
	}

	return nil
}

func tagPublicKey(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	var errBool bool
	for _, nodeInfo := range gcpCluster.NodeInfos {
		instanceName := nodeInfo.Instance.Name
		newMetadata := *nodeInfo.Instance.Metadata
		keyVal := fmt.Sprintf("%s:%s %s@%s",
			gcpCluster.GCPProfile.ServiceAccount,
			strings.Replace(gcpCluster.KeyPair.Pub, "\n", "", -1),
			gcpCluster.GCPProfile.ServiceAccount,
			instanceName)
		newMetadata.Items = append(newMetadata.Items, &compute.MetadataItems{
			Key:   "sshKeys",
			Value: &keyVal,
		})
		_, err := computeSvc.Instances.
			SetMetadata(projectId, gcpCluster.Zone, instanceName, &newMetadata).
			Do()
		if err != nil {
			errBool = true
		}
	}
	if errBool {
		return errors.New("Unable to tag sshKeys for all node")
	}

	return nil
}

func waitUntilClusterCreateComplete(
	containerSvc *container.Service,
	projectId string,
	zone string,
	clusterId string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		resp, err := containerSvc.Projects.Zones.Clusters.NodePools.
			List(projectId, zone, clusterId).
			Do()
		if err != nil {
			return false, nil
		}
		if resp.NodePools[0].Status == "RUNNING" {
			log.Info("Create cluster complete")
			return true, nil
		}
		return false, nil
	})
}

func waitUntilClusterDeleteComplete(
	containerSvc *container.Service,
	projectId string,
	zone string,
	clusterId string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		_, err := containerSvc.Projects.Zones.Clusters.NodePools.
			List(projectId, zone, clusterId).
			Do()
		if err != nil && strings.Contains(err.Error(), "was not found") {
			log.Info("Delete cluster complete")
			return true, nil
		}
		return false, nil
	})
}

func waitUntilNodePoolCreateComplete(
	containerSvc *container.Service,
	projectId string,
	zone string,
	clusterId string,
	nodePoolIds []string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		for _, nodePoolId := range nodePoolIds {
			_, err := containerSvc.Projects.Zones.Clusters.NodePools.
				Get(projectId, zone, clusterId, nodePoolId).
				Do()
			if err != nil {
				return false, nil
			}
		}
		log.Info("Node pool create complete")
		return true, nil
	})
}
