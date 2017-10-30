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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var publicPortType = 1

func NewNodePoolRequest(
	deploymentName string,
	machineType string,
	nodePoolSize int) (string, *container.CreateNodePoolRequest) {
	nodePoolName := createUniqueNodePoolName(machineType, nodePoolSize)
	return nodePoolName, &container.CreateNodePoolRequest{
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
				Metadata: map[string]string{
					"deploymentName": deploymentName,
					"nodePoolName":   nodePoolName,
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

func createUniqueNodePoolName(machineType string, nodePoolSize int) string {
	timeSeq := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("%s-%d-%s", machineType, nodePoolSize, timeSeq)
}

func CreateNodePools(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	deployment *apis.Deployment,
	log *logging.Logger,
	groupByNodeInstanceType bool) ([]string, error) {
	containerSvc, err := container.New(client)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	nodePoolIds := []string{}
	if groupByNodeInstanceType {
		instanceTypesMap := map[string]int{}
		for _, node := range deployment.ClusterDefinition.Nodes {
			cnt, ok := instanceTypesMap[node.InstanceType]
			if ok {
				instanceTypesMap[node.InstanceType] = (cnt + 1)
			} else {
				instanceTypesMap[node.InstanceType] = 1
			}
		}

		for instanceType, cnt := range instanceTypesMap {
			nodePoolName, err := createClusterNodePool(containerSvc, projectId, zone, clusterId, instanceType, cnt,
				deployment, log)
			if err != nil {
				deleteNodePools(client, projectId, zone, clusterId, []string{nodePoolName}, log)
				return nil, fmt.Errorf("Unable to create %s node pool: %s", nodePoolName, err.Error())
			}
			nodePoolIds = append(nodePoolIds, nodePoolName)
		}
	} else {
		nodePoolSize := len(deployment.ClusterDefinition.Nodes)
		instanceType := deployment.ClusterDefinition.Nodes[0].InstanceType
		nodePoolName, err := createClusterNodePool(containerSvc, projectId, zone, clusterId,
			instanceType, nodePoolSize, deployment, log)
		if err != nil {
			deleteNodePools(client, projectId, zone, clusterId, []string{nodePoolName}, log)
			return nil, fmt.Errorf("Unable to create %s node pool: %s", nodePoolName, err.Error())
		}
		nodePoolIds = append(nodePoolIds, nodePoolName)
	}

	return nodePoolIds, nil
}

func createClusterNodePool(
	containerSvc *container.Service,
	projectId string,
	zone string,
	clusterId string,
	instanceType string,
	nodePoolSize int,
	deployment *apis.Deployment,
	log *logging.Logger) (string, error) {
	nodePoolName, nodePoolRequest := NewNodePoolRequest(deployment.Name, instanceType, nodePoolSize)
	_, err := containerSvc.Projects.Zones.Clusters.NodePools.
		Create(projectId, zone, clusterId, nodePoolRequest).
		Do()
	if err != nil {
		return "", fmt.Errorf("Unable to create %s node pool: %s", nodePoolName, err.Error())
	}

	log.Infof("Waiting until %s node pool is completed...", nodePoolName)
	if err := waitUntilNodePoolCreateComplete(containerSvc, projectId, zone,
		clusterId, nodePoolName, time.Duration(10)*time.Minute, log); err != nil {
		return "", fmt.Errorf("Unable to wait until %s node pool to be complete: %s\n",
			nodePoolName, err.Error())
	}

	return nodePoolName, nil
}

func deleteNodePools(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	nodePoolIds []string,
	log *logging.Logger) error {
	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}

	var errBool bool
	for _, nodePoolId := range nodePoolIds {
		log.Infof("Deleting %s node pool: %s", clusterId, nodePoolId)
		_, err = containerSvc.Projects.Zones.Clusters.NodePools.
			Delete(projectId, zone, clusterId, nodePoolId).
			Do()
		if err != nil {
			log.Warningf("Unable to delete %s node pool: %s", nodePoolId, err.Error())
			errBool = true
		}
	}
	if errBool {
		return fmt.Errorf("Unable to delete %s node pools", clusterId)
	}

	return nil
}

func deleteFirewallRules(
	client *http.Client,
	projectId string,
	zone string,
	log *logging.Logger) error {
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	fireWallNames := []string{}
	firewallsResp, err := computeSvc.Firewalls.List(projectId).Do()
	if err != nil {
		return errors.New("Unable to list firewalls: " + err.Error())
	}
	for _, item := range firewallsResp.Items {
		if strings.HasPrefix(item.Name, "gke-") && strings.HasSuffix(item.Name, "-http") {
			fireWallNames = append(fireWallNames, item.Name)
		}
	}

	needDeletefireWallNames := []string{}
	instancesResp, err := computeSvc.Instances.List(projectId, zone).Do()
	for _, fireWallName := range fireWallNames {
		findUsed := false
		for _, instance := range instancesResp.Items {
			nodeIp := strings.Replace(instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, ".", "-", -1)
			if fireWallName == fmt.Sprintf("gke-%s-http", nodeIp) {
				findUsed = true
			}
		}
		if !findUsed {
			needDeletefireWallNames = append(needDeletefireWallNames, fireWallName)
		}
	}

	var errBool bool
	for _, firewallName := range needDeletefireWallNames {
		_, err = computeSvc.Firewalls.Delete(projectId, firewallName).Do()
		if err != nil {
			if strings.Contains(err.Error(), "was not found") {
				log.Warningf("Unable to find %s to be delete", firewallName)
				continue
			}
			log.Warningf("Unable to delete firewall rules: " + err.Error())
			errBool = true
		}
	}
	if errBool {
		return fmt.Errorf("Unable to delete firewall rules")
	}

	return nil
}

func populateNodeInfos(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	nodePoolIds []string,
	gcpCluster *hpgcp.GCPCluster,
	clusterNodeDefinition apis.ClusterDefinition,
	log *logging.Logger) error {
	instanceGroupNames := []string{}
	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	log.Infof("Populate nodeInfos with nodePoolIds: %s", nodePoolIds)
	for _, nodePoolId := range nodePoolIds {
		resp, err := containerSvc.Projects.Zones.Clusters.NodePools.
			Get(projectId, zone, clusterId, nodePoolId).
			Do()
		if err != nil {
			return fmt.Errorf("Unable to get %s node pool: %s", nodePoolId, err.Error())
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

	clusterInstancesMap := map[int]*compute.Instance{}
	for i, instanceName := range instanceNames {
		instance, err := computeSvc.Instances.Get(projectId, zone, instanceName).Do()
		if err != nil {
			return fmt.Errorf("Unable to get %s instances: %s", instanceName, err.Error())
		}
		clusterInstancesMap[i] = instance
		log.Infof("Find %s node instance name: %s, instance type: %s",
			gcpCluster.ClusterId, instanceName, instance.MachineType)
	}

	for _, node := range clusterNodeDefinition.Nodes {
		assignedKey := -1
		for key, instance := range clusterInstancesMap {
			machineTypeUrls := strings.Split(instance.MachineType, "/")
			if node.InstanceType == machineTypeUrls[len(machineTypeUrls)-1] {
				log.Infof("Assigning public dns name %s to node %d", instance.Name, node.Id)
				gcpCluster.NodeInfos[node.Id] = &hpgcp.NodeInfo{
					Instance:  instance,
					PublicIp:  instance.NetworkInterfaces[0].AccessConfigs[0].NatIP,
					PrivateIp: instance.NetworkInterfaces[0].NetworkIP,
				}
				assignedKey = key
				break
			}
		}
		if assignedKey != -1 {
			delete(clusterInstancesMap, assignedKey)
		}
	}

	if len(gcpCluster.NodeInfos) != len(clusterNodeDefinition.Nodes) {
		return fmt.Errorf("Unexpected list of nodeInfos: %d", len(gcpCluster.NodeInfos))
	}

	return nil
}

func reloadNodeInfos(
	client *http.Client,
	kubeConfig *rest.Config,
	projectId string,
	zone string,
	clusterId string,
	gcpCluster *hpgcp.GCPCluster,
	log *logging.Logger) error {
	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to create in cluster k8s client: " + err.Error())
	}

	nodes, err := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return errors.New("Unable to list kubernetes nodes: " + err.Error())
	}

	nodeNames := map[int]string{}
	for _, node := range nodes.Items {
		if deployment, ok := node.Labels["hyperpilot/deployment"]; ok {
			if clusterId == deployment {
				if nodeId, ok := node.Labels["hyperpilot/node-id"]; ok {
					id, err := strconv.Atoi(nodeId)
					if err != nil {
						return errors.New("Unable to convert node id to int: " + err.Error())
					}
					nodeNames[id] = node.GetName()
				}
			}
		}
	}

	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	for nodeId, nodeName := range nodeNames {
		instance, err := computeSvc.Instances.Get(projectId, zone, nodeName).Do()
		if err != nil {
			return errors.New("Unable to get compute instance: " + err.Error())
		}
		gcpCluster.NodeInfos[nodeId] = &hpgcp.NodeInfo{
			Instance:  instance,
			PublicIp:  instance.NetworkInterfaces[0].AccessConfigs[0].NatIP,
			PrivateIp: instance.NetworkInterfaces[0].NetworkIP,
		}
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

func insertFirewallIngressRules(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	for nodeId, nodeInfo := range gcpCluster.NodeInfos {
		allowedPorts := getAllowedPortsByNodeTasks(nodeId, deployment, log)
		nodeIp := strings.Replace(nodeInfo.PublicIp, ".", "-", -1)
		firewallName := fmt.Sprintf("gke-%s-http", nodeIp)
		targetTagName := fmt.Sprintf("gke-%s-http-server", nodeIp)
		tagFirewall := &compute.Firewall{
			Allowed: []*compute.FirewallAllowed{
				&compute.FirewallAllowed{
					IPProtocol: "tcp",
					Ports:      allowedPorts,
				},
			},
			Description: "INGRESS",
			Name:        firewallName,
			Priority:    int64(1000),
			TargetTags:  []string{targetTagName},
		}
		if _, err := computeSvc.Firewalls.Insert(projectId, tagFirewall).Do(); err != nil {
			return errors.New("Unable to insert firewall ingress rules: " + err.Error())
		}

		if err := tagFirewallTarget(computeSvc, gcpCluster, targetTagName, log); err != nil {
			return errors.New("Unable to tag firewall target to node instance: " + err.Error())
		}
	}

	return nil
}

func updateFirewallIngressRules(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	for nodeId, nodeInfo := range gcpCluster.NodeInfos {
		allowedPorts := getAllowedPortsByNodeTasks(nodeId, deployment, log)
		nodeIp := strings.Replace(nodeInfo.PublicIp, ".", "-", -1)
		firewallName := fmt.Sprintf("gke-%s-http", nodeIp)
		targetTagName := fmt.Sprintf("gke-%s-http-server", nodeIp)
		tagFirewall := &compute.Firewall{
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
		}
		if _, err := computeSvc.Firewalls.Update(projectId, firewallName, tagFirewall).Do(); err != nil {
			return errors.New("Unable to update firewall ingress rules: " + err.Error())
		}
	}

	return nil
}

func getAllowedPortsByNodeTasks(nodeId int, deployment *apis.Deployment, log *logging.Logger) []string {
	allowedPorts := []string{}
	allowedPortTasks := []string{}
	for _, node := range deployment.NodeMapping {
		if node.Id == nodeId {
			allowedPortTasks = append(allowedPortTasks, node.Task)
		}
	}

	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.PortTypes == nil || len(task.PortTypes) == 0 {
			continue
		}

		isNodeTask := false
		for _, nodeTaskName := range allowedPortTasks {
			if task.Family == nodeTaskName {
				isNodeTask = true
			}
		}
		if !isNodeTask {
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

	return allowedPorts
}

func tagFirewallTarget(
	computeSvc *compute.Service,
	gcpCluster *hpgcp.GCPCluster,
	targetTagName string,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	var errBool bool
	for _, nodeInfo := range gcpCluster.NodeInfos {
		instanceName := nodeInfo.Instance.Name
		instance, err := computeSvc.Instances.Get(projectId, gcpCluster.Zone, instanceName).Do()
		if err != nil {
			log.Warningf("Unable to get %s instance: %s", instanceName, err.Error())
			errBool = true
			break
		}

		instance.Tags.Items = append(instance.Tags.Items, targetTagName)
		_, err = computeSvc.Instances.
			SetTags(projectId, gcpCluster.Zone, instanceName, instance.Tags).
			Do()
		if err != nil {
			log.Warningf("Unable to tag network: " + err.Error())
			errBool = true
		}
	}
	if errBool {
		return errors.New("Unable to tag network for all node")
	}

	return nil
}

func tagPublicKey(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	serviceAccount := gcpCluster.GCPProfile.ServiceAccount
	publicKey := gcpCluster.KeyPair.Pub
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	resp, err := computeSvc.Projects.Get(projectId).Do()
	if err != nil {
		return errors.New("Unable to get projects metadata: " + err.Error())
	}

	orignalKeyVal := ""
	sshKeyItemIndex := -1
	for i, item := range resp.CommonInstanceMetadata.Items {
		if item.Key == "ssh-keys" {
			orignalKeyVal = *item.Value
			sshKeyItemIndex = i
			break
		}
	}

	newSshKeyVal := fmt.Sprintf("%s:%s %s@%s", serviceAccount, publicKey, serviceAccount, serviceAccount)
	if sshKeyItemIndex == -1 {
		resp.CommonInstanceMetadata.Items = append(resp.CommonInstanceMetadata.Items, &compute.MetadataItems{
			Key:   "ssh-keys",
			Value: &newSshKeyVal,
		})
	} else {
		sshKeys := strings.Split(orignalKeyVal, "\n")
		for _, sshKey := range sshKeys {
			if strings.HasPrefix(sshKey, serviceAccount+":") && strings.HasSuffix(sshKey, serviceAccount+"@"+serviceAccount) {
				log.Infof("Share serviceAccout %s publicKey has been tag", serviceAccount)
				return nil
			}
		}

		keyVal := orignalKeyVal + "\n" + newSshKeyVal
		resp.CommonInstanceMetadata.Items[sshKeyItemIndex].Value = &keyVal
	}

	_, err = computeSvc.Projects.SetCommonInstanceMetadata(projectId, resp.CommonInstanceMetadata).Do()
	if err != nil {
		return errors.New("Unable to set public key to projects metadata: " + err.Error())
	}

	return nil
}

func findDeploymentNames(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string) ([]string, error) {
	computeSvc, err := compute.New(client)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	resp, err := computeSvc.Instances.List(projectId, zone).Do()
	if err != nil {
		return nil, errors.New("Unable to list instance: " + err.Error())
	}

	clusterInstances := []*compute.Instance{}
	for _, instance := range resp.Items {
		for _, metadata := range instance.Metadata.Items {
			if metadata.Key == "cluster-name" && *metadata.Value == clusterId {
				clusterInstances = append(clusterInstances, instance)
			}
		}
	}

	deploymentNames := []string{}
	for _, instance := range clusterInstances {
		for _, metadata := range instance.Metadata.Items {
			if metadata.Key == "deploymentName" {
				existBool := false
				for _, deploymentName := range deploymentNames {
					if deploymentName == *metadata.Value {
						existBool = true
					}
				}
				if !existBool {
					deploymentNames = append(deploymentNames, *metadata.Value)
				}
			}
		}
	}

	return deploymentNames, nil
}

func findNodePoolNames(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	deploymentName string) ([]string, error) {
	computeSvc, err := compute.New(client)
	if err != nil {
		return nil, errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	resp, err := computeSvc.Instances.List(projectId, zone).Do()
	if err != nil {
		return nil, errors.New("Unable to list instance: " + err.Error())
	}

	clusterInstances := []*compute.Instance{}
	for _, instance := range resp.Items {
		for _, metadata := range instance.Metadata.Items {
			if metadata.Key == "cluster-name" && *metadata.Value == clusterId {
				clusterInstances = append(clusterInstances, instance)
			}
		}
	}

	deploymentInstances := []*compute.Instance{}
	for _, instance := range clusterInstances {
		for _, metadata := range instance.Metadata.Items {
			if metadata.Key == "deploymentName" && *metadata.Value == deploymentName {
				deploymentInstances = append(deploymentInstances, instance)
			}
		}
	}

	nodePoolNames := []string{}
	for _, instance := range deploymentInstances {
		for _, metadata := range instance.Metadata.Items {
			if metadata.Key == "nodePoolName" {
				existBool := false
				for _, nodePoolName := range nodePoolNames {
					if nodePoolName == *metadata.Value {
						existBool = true
					}
				}
				if !existBool {
					nodePoolNames = append(nodePoolNames, *metadata.Value)
				}
			}
		}
	}

	return nodePoolNames, nil
}

func waitUntilClusterStatusRunning(
	containerSvc *container.Service,
	projectId string,
	zone string,
	clusterId string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		resp, err := containerSvc.Projects.Zones.Clusters.
			Get(projectId, zone, clusterId).
			Do()
		if err != nil {
			return false, nil
		}
		if resp.Status == "RUNNING" {
			log.Info("Cluster status is RUNNING")
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
	nodePoolName string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		nodePool, _ := containerSvc.Projects.Zones.Clusters.NodePools.
			Get(projectId, zone, clusterId, nodePoolName).
			Do()
		if nodePool == nil {
			return false, nil
		}
		log.Info("Node pool create complete")
		return true, nil
	})
}

func waitUntilNodePoolDeleteComplete(
	client *http.Client,
	projectId string,
	zone string,
	clusterId string,
	nodePoolIds []string,
	timeout time.Duration,
	log *logging.Logger) error {
	containerSvc, err := container.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform container service: " + err.Error())
	}
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		for _, nodePoolId := range nodePoolIds {
			nodePoll, _ := containerSvc.Projects.Zones.Clusters.NodePools.
				Get(projectId, zone, clusterId, nodePoolId).
				Do()
			if nodePoll != nil {
				return false, nil
			}
		}
		log.Info("Delete node pool complete")
		return true, nil
	})
}
