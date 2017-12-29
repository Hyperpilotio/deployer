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
	nodeNames := map[int]string{}
	for _, node := range deployment.ClusterDefinition.Nodes {
		instanceName := gcpCluster.NodeInfos[node.Id].Instance.Name
		nodeNames[node.Id] = instanceName
	}

	return k8sUtil.TagKubeNodes(k8sClient, deployment.Name, deployment.ClusterDefinition, nodeNames, log)
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

	allowedPorts := getDeploymentAllowedPorts(deployment, log)
	firewallName := fmt.Sprintf("gke-%s-http", gcpCluster.ClusterId)
	targetTagName := fmt.Sprintf("gke-%s-http-server", gcpCluster.ClusterId)
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

	allowedPorts := getDeploymentAllowedPorts(deployment, log)
	firewallName := fmt.Sprintf("gke-%s-http", gcpCluster.ClusterId)
	targetTagName := fmt.Sprintf("gke-%s-http-server", gcpCluster.ClusterId)
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

	return nil
}

func deleteFirewallRules(
	client *http.Client,
	projectId string,
	firewallName string,
	log *logging.Logger) error {
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	_, err = computeSvc.Firewalls.Delete(projectId, firewallName).Do()
	if err != nil {
		if strings.Contains(err.Error(), "was not found") {
			log.Warningf("Unable to find %s to be delete", firewallName)
			return nil
		}
		return errors.New("Unable to delete firewall rules: " + err.Error())
	}

	return nil
}

func deleteLoadBalancing(
	client *http.Client,
	gcpCluster *hpgcp.GCPCluster,
	log *logging.Logger) error {
	projectId := gcpCluster.GCPProfile.ProjectId
	computeSvc, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	regions := strings.Split(gcpCluster.Zone, "-")
	region := strings.Join(regions[:len(regions)-1], "-")
	resp, err := computeSvc.TargetPools.List(projectId, region).Do()
	if err != nil {
		return errors.New("Unable to list targetPools: " + err.Error())
	}

	k8sUsedFirewalls := []string{}
	firewallsResp, err := computeSvc.Firewalls.List(projectId).Do()
	if err != nil {
		return errors.New("Unable to list firewalls: " + err.Error())
	}
	for _, item := range firewallsResp.Items {
		if strings.HasPrefix(item.Name, "k8s-") {
			for _, targetTag := range item.TargetTags {
				if strings.Index(targetTag, gcpCluster.ClusterId) != -1 {
					k8sUsedFirewalls = append(k8sUsedFirewalls, item.Name)
				}
			}
		}
	}

	healthCheckNames := []string{}
	targetPools := []string{}
	for _, targetPool := range resp.Items {
		clusterUsed := false
		for _, nodeInfo := range gcpCluster.NodeInfos {
			for _, instanceUrl := range targetPool.Instances {
				if strings.Index(instanceUrl, nodeInfo.Instance.Name) != -1 {
					clusterUsed = true
				}
			}
		}

		if clusterUsed {
			for _, healthCheckUrl := range targetPool.HealthChecks {
				urls := strings.Split(healthCheckUrl, "/")
				healthCheckNames = append(healthCheckNames, urls[len(urls)-1])
			}
			targetPools = append(targetPools, targetPool.Name)
		}
	}

	log.Infof("Deleting kubernetes used firewalls: %s", k8sUsedFirewalls)
	errMsgs := []string{}
	var errBool bool
	for _, firewallName := range k8sUsedFirewalls {
		_, err = computeSvc.Firewalls.Delete(projectId, firewallName).Do()
		if err != nil {
			errMsgs = append(errMsgs, "Unable to delete firewall: "+err.Error())
			errBool = true
		}
	}

	log.Infof("Deleting targetPools: %s", targetPools)
	for _, targetPool := range targetPools {
		_, err := computeSvc.ForwardingRules.Delete(projectId, region, targetPool).Do()
		if err != nil {
			errMsgs = append(errMsgs, "Unable to delete ForwardingRule: "+err.Error())
			errBool = true
		}

		err = waitUntilForwardingRulesDeleteComplete(computeSvc, projectId, region,
			targetPool, time.Duration(30)*time.Second, log)
		if err != nil {
			errMsgs = append(errMsgs, "Unable to wait until forwardingRules to be delete completed: "+err.Error())
			errBool = true
		}

		_, err = computeSvc.TargetPools.Delete(projectId, region, targetPool).Do()
		if err != nil {
			errMsgs = append(errMsgs, "Unable to delete targetPool: "+err.Error())
			errBool = true
		}

		err = waitUntilTargetPoolsDeleteComplete(computeSvc, projectId, region,
			targetPool, time.Duration(30)*time.Second, log)
		if err != nil {
			errMsgs = append(errMsgs, "Unable to wait until targetPools to be delete completed: "+err.Error())
			errBool = true
		}
	}

	log.Infof("Deleting healthCheck: %s", healthCheckNames)
	for _, healthCheckName := range healthCheckNames {
		_, err := computeSvc.HttpHealthChecks.Delete(projectId, healthCheckName).Do()
		if err != nil {
			errMsgs = append(errMsgs, "Unable to delete healthCheck: "+err.Error())
			errBool = true
		}
	}

	if errBool {
		return errors.New(strings.Join(errMsgs, ": "))
	}

	return nil
}

func getDeploymentAllowedPorts(deployment *apis.Deployment, log *logging.Logger) []string {
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

	return allowedPorts
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

func waitUntilForwardingRulesDeleteComplete(
	computeSvc *compute.Service,
	projectId string,
	region string,
	forwardingRuleName string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		_, err := computeSvc.ForwardingRules.Get(projectId, region, forwardingRuleName).Do()
		if err != nil && strings.Contains(err.Error(), "was not found") {
			log.Info("Delete forwardingRule complete")
			return true, nil
		}
		return false, nil
	})
}

func waitUntilTargetPoolsDeleteComplete(
	computeSvc *compute.Service,
	projectId string,
	region string,
	targetPoolName string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		_, err := computeSvc.TargetPools.Get(projectId, region, targetPoolName).Do()
		if err != nil && strings.Contains(err.Error(), "was not found") {
			log.Info("Delete targetPool complete")
			return true, nil
		}
		return false, nil
	})
}
