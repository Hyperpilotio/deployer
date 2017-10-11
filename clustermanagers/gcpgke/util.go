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

func populateNodeInfos(client *http.Client, gcpCluster *hpgcp.GCPCluster) error {
	computeSrv, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	resp, err := computeSrv.Instances.
		List(gcpCluster.GCPProfile.ProjectId, gcpCluster.Zone).
		Do()
	if err != nil {
		return fmt.Errorf("Unable to %s list instances: %s", gcpCluster.ClusterId, err.Error())
	}

	clusterInstances := []*compute.Instance{}
	for _, instance := range resp.Items {
		for _, item := range instance.Metadata.Items {
			if item.Key == "cluster-name" && *item.Value == gcpCluster.ClusterId {
				clusterInstances = append(clusterInstances, instance)
				break
			}
		}
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
	computeSrv, err := compute.New(client)
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
	_, err = computeSrv.Firewalls.Insert(projectId, &compute.Firewall{
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
		_, err := computeSrv.Instances.
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
	computeSrv, err := compute.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform compute service: " + err.Error())
	}

	_, err = computeSrv.Firewalls.Delete(projectId, firewallName).Do()
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
	computeSrv, err := compute.New(client)
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
		_, err := computeSrv.Instances.
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
	containerSrv *container.Service,
	projectId string,
	zone string,
	clusterId string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		resp, err := containerSrv.Projects.Zones.Clusters.NodePools.
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
	containerSrv *container.Service,
	projectId string,
	zone string,
	clusterId string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		_, err := containerSrv.Projects.Zones.Clusters.NodePools.
			List(projectId, zone, clusterId).
			Do()
		if err != nil && strings.Contains(err.Error(), "was not found") {
			log.Info("Delete cluster complete")
			return true, nil
		}
		return false, nil
	})
}
