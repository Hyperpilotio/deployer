package gcpgke

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
	"k8s.io/client-go/rest"

	"github.com/golang/glog"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/funcs"
	"github.com/hyperpilotio/go-utils/log"
)

var publicPortType = 1

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
	projectId, err := getProjectId(gcpCluster.GCPProfile.ServiceAccountPath)
	if err != nil {
		return nil, errors.New("Unable to find projectId: " + err.Error())
	}
	gcpCluster.GCPProfile.ProjectId = projectId

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

func (gcpDeployer *GCPDeployer) DownloadKubeConfig() error {
	baseDir := gcpDeployer.GCPCluster.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	// TODO write kubeconfig to kubeconfigFilePath

	gcpDeployer.KubeConfigPath = kubeconfigFilePath
	return nil
}

func deployCluster(gcpDeployer *GCPDeployer, uploadedFiles map[string]string) error {
	gcpCluster := gcpDeployer.GCPCluster
	deployment := gcpDeployer.Deployment
	log := gcpDeployer.DeploymentLog.Logger
	client, err := hpgcp.CreateClient(gcpCluster.GCPProfile, gcpCluster.Zone)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	if err := deployKubernetes(client, gcpDeployer); err != nil {
		return errors.New("Unable to deploy kubernetes custer: " + err.Error())
	}

	if err := populateNodeInfos(client, gcpCluster); err != nil {
		return errors.New("Unable to populate node infos: " + err.Error())
	}

	k8sClient, err := k8s.NewForConfig(gcpDeployer.KubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	if err := tagKubeNodes(k8sClient, gcpCluster, deployment, log); err != nil {
		// deleteDeploymentOnFailure(k8sDeployer)
		return errors.New("Unable to tag Kubernetes nodes: " + err.Error())
	}

	if err := gcpDeployer.deployKubernetesObjects(k8sClient, false); err != nil {
		// deleteDeploymentOnFailure(k8sDeployer)
		return errors.New("Unable to deploy kubernetes objects: " + err.Error())
	}

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
			gcpDeployer.KubeConfig = config
		}
	}

	if err := gcpDeployer.DownloadKubeConfig(); err != nil {
		return errors.New("Unable to download kubeconfig: " + err.Error())
	}
	log.Infof("Downloaded kube config at %s", gcpDeployer.KubeConfigPath)

	return nil
}

func (gcpDeployer *GCPDeployer) deployKubernetesObjects(k8sClient *k8s.Clientset, skipDelete bool) error {
	namespaces, namespacesErr := getExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		if !skipDelete {
			// deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := createSecrets(k8sClient, namespaces, gcpDeployer.Deployment); err != nil {
		if !skipDelete {
			// deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := gcpDeployer.deployServices(k8sClient, namespaces); err != nil {
		if !skipDelete {
			// deleteDeploymentOnFailure(k8sDeployer)
		}
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	// k8sDeployer.recordPublicEndpoints(k8sClient)

	return nil
}

func getExistingNamespaces(k8sClient *k8s.Clientset) (map[string]bool, error) {
	namespaces := map[string]bool{}
	k8sNamespaces := k8sClient.CoreV1().Namespaces()
	existingNamespaces, err := k8sNamespaces.List(metav1.ListOptions{})
	if err != nil {
		return namespaces, fmt.Errorf("Unable to get existing namespaces: " + err.Error())
	}

	for _, existingNamespace := range existingNamespaces.Items {
		namespaces[existingNamespace.Name] = true
	}

	return namespaces, nil
}

func getNamespace(objectMeta metav1.ObjectMeta) string {
	namespace := objectMeta.Namespace
	if namespace == "" {
		return "default"
	}

	return namespace
}

func createNamespaceIfNotExist(namespace string, existingNamespaces map[string]bool, k8sClient *k8s.Clientset) error {
	if _, ok := existingNamespaces[namespace]; !ok {
		glog.Infof("Creating new namespace %s", namespace)
		k8sNamespaces := k8sClient.CoreV1().Namespaces()
		_, err := k8sNamespaces.Create(&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})
		if err != nil {
			return fmt.Errorf("Unable to create namespace '%s': %s", namespace, err.Error())
		}
		existingNamespaces[namespace] = true
	}

	return nil
}

func createSecrets(k8sClient *k8s.Clientset, existingNamespaces map[string]bool, deployment *apis.Deployment) error {
	secrets := deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	for _, secret := range secrets {
		namespace := getNamespace(secret.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
		}

		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func createServiceForDeployment(namespace string, family string, k8sClient *k8s.Clientset,
	task apis.KubernetesTask, container v1.Container, log *logging.Logger) error {
	if len(container.Ports) == 0 {
		return nil
	}

	service := k8sClient.CoreV1().Services(namespace)
	serviceName := family
	if !strings.HasPrefix(family, serviceName) {
		serviceName = serviceName + "-" + container.Name
	}
	labels := map[string]string{"app": family}
	servicePorts := []v1.ServicePort{}
	for i, port := range container.Ports {
		newPort := v1.ServicePort{
			Port:       port.HostPort,
			TargetPort: intstr.FromInt(int(port.ContainerPort)),
			Name:       "port" + strconv.Itoa(i),
		}
		servicePorts = append(servicePorts, newPort)
	}

	internalService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeClusterIP,
			Ports:    servicePorts,
			Selector: labels,
		},
	}
	_, err := service.Create(internalService)
	if err != nil {
		return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
	}
	log.Infof("Created %s internal service", serviceName)

	// Check the type of each port opened by the container; create a loadbalancer service to expose the public port
	if task.PortTypes == nil || len(task.PortTypes) == 0 {
		return nil
	}

	for i, portType := range task.PortTypes {
		if portType != publicPortType {
			log.Infof("Skipping creating public endpoint for service %s as it's marked as private", serviceName)
			continue
		}
		// public port
		publicServiceName := serviceName + "-public" + servicePorts[i].Name
		publicService := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      publicServiceName,
				Labels:    labels,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{
					v1.ServicePort{
						Port:       servicePorts[i].Port,
						TargetPort: servicePorts[i].TargetPort,
						Name:       "public-" + servicePorts[i].Name,
					},
				},
				Selector: labels,
			},
		}
		_, err := service.Create(publicService)
		if err != nil {
			return fmt.Errorf("Unable to create public service %s: %s", publicServiceName, err)
		}

		log.Infof("Created a public service %s with port %d", publicServiceName, servicePorts[i].Port)
	}

	return nil
}

func (gcpDeployer *GCPDeployer) deployServices(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
	deployment := gcpDeployer.Deployment
	kubeConfig := gcpDeployer.KubeConfig
	log := gcpDeployer.DeploymentLog.Logger

	if kubeConfig == nil {
		return errors.New("Unable to find kube config in deployment")
	}

	tasks := map[string]apis.KubernetesTask{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	taskCount := map[string]int{}

	// We sort before we create services because we want to have a deterministic way to assign
	// service ids
	sort.Sort(deployment.NodeMapping)

	for _, mapping := range deployment.NodeMapping {
		log.Infof("Deploying task %s with mapping %d", mapping.Task, mapping.Id)

		task, ok := tasks[mapping.Task]
		if !ok {
			return fmt.Errorf("Unable to find task %s in task definitions", mapping.Task)
		}

		deploySpec := task.Deployment
		if deploySpec == nil {
			return fmt.Errorf("Unable to find deployment in task %s", mapping.Task)
		}
		family := task.Family
		namespace := getNamespace(deploySpec.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		originalFamily := family
		count, ok := taskCount[family]

		if !ok {
			count = 1
			deploySpec.Name = originalFamily
			deploySpec.Labels["app"] = originalFamily
			deploySpec.Spec.Template.Labels["app"] = originalFamily
		} else {
			// Update deploy spec to reflect multiple count of the same task
			count += 1
			family = family + "-" + strconv.Itoa(count)
			deploySpec.Name = family
			deploySpec.Labels["app"] = family
			deploySpec.Spec.Template.Labels["app"] = family
		}

		if deploySpec.Spec.Selector != nil {
			deploySpec.Spec.Selector.MatchLabels = deploySpec.Spec.Template.Labels
		}

		// Public Url will be tagged later in recordPublicEndpoint post deployment
		// k8sDeployer.Services[family] = ServiceMapping{
		// 	NodeId: mapping.Id,
		// }

		taskCount[originalFamily] = count

		// Assigning Pods to Nodes
		nodeSelector := map[string]string{}
		log.Infof("Selecting node %d for deployment %s", mapping.Id, family)

		nodeSelector["hyperpilot/node-id"] = strconv.Itoa(mapping.Id)

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := createServiceForDeployment(namespace, family, k8sClient, task, container, log)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		deploy := k8sClient.Extensions().Deployments(namespace)
		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unable to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		if task.Deployment != nil {
			return fmt.Errorf("Cannot assign both daemonset and deployment to the same task: %s", task.Family)
		}

		daemonSet := task.DaemonSet
		namespace := getNamespace(daemonSet.ObjectMeta)
		if err := createNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	clusterRole := k8sClient.RbacV1beta1().ClusterRoles()
	nodeReader := &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "node-reader"},
		Rules: []rbac.PolicyRule{
			rbac.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "watch", "list"},
			},
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRole",
			APIVersion: "rbac.authorization.k8s.io/v1beta1",
		},
	}
	if _, err := clusterRole.Create(nodeReader); err != nil {
		log.Warningf("Unable to create role 'node-reader': %s", err.Error())
	}

	clusterRoleBindings := k8sClient.RbacV1beta1().ClusterRoleBindings()
	hyperpilotRoleBinding := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "hyperpilot-cluster-role"},
		RoleRef: rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbac.Subject{
			rbac.Subject{
				Kind:      rbac.ServiceAccountKind,
				Name:      "default",
				Namespace: "hyperpilot",
			},
		},
	}
	if _, err := clusterRoleBindings.Create(hyperpilotRoleBinding); err != nil {
		log.Warningf("Unable to create hyperpilot role binding: " + err.Error())
	}

	defaultRoleBinding := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "default-cluster-role"},
		RoleRef: rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbac.Subject{
			rbac.Subject{
				Kind:      rbac.ServiceAccountKind,
				Name:      "default",
				Namespace: "default",
			},
		},
	}
	if _, err := clusterRoleBindings.Create(defaultRoleBinding); err != nil {
		log.Warningf("Unable to create default role binding: " + err.Error())
	}

	return nil
}

func (gcpDeployer *GCPDeployer) recordPublicEndpoints(k8sClient *k8s.Clientset) {
	deployment := gcpDeployer.Deployment
	log := gcpDeployer.DeploymentLog.Logger
	// gcpDeployer.Services = map[string]ServiceMapping{}

	allNamespaces := getAllDeployedNamespaces(deployment)
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		tagElbFunc := func() {
			allElbsTagged := true
			for _, namespace := range allNamespaces {
				services := k8sClient.CoreV1().Services(namespace)
				serviceLists, listError := services.List(metav1.ListOptions{})
				if listError != nil {
					log.Warningf("Unable to list services for namespace '%s': %s", namespace, listError.Error())
					return
				}
				for _, service := range serviceLists.Items {
					serviceName := service.GetObjectMeta().GetName()
					if strings.Index(serviceName, "-public") != -1 {
						if len(service.Status.LoadBalancer.Ingress) > 0 {
							// hostname := service.Status.LoadBalancer.Ingress[0].Hostname
							// port := service.Spec.Ports[0].Port

							// serviceMapping := ServiceMapping{
							// 	PublicUrl: hostname + ":" + strconv.FormatInt(int64(port), 10),
							// }

							// familyName := serviceName[:strings.Index(serviceName, "-public")]
							// if mapping, ok := k8sDeployer.Services[familyName]; ok {
							// 	serviceMapping.NodeId = mapping.NodeId
							// }
							// k8sDeployer.Services[familyName] = serviceMapping
						} else {
							allElbsTagged = false
							break
						}
					}
				}

				if allElbsTagged {
					c <- true
				} else {
					time.Sleep(time.Second * 2)
				}
			}
		}

		for {
			select {
			case <-quit:
				return
			default:
				tagElbFunc()
			}
		}
	}()

	select {
	case <-c:
		log.Info("All public endpoints recorded.")
	case <-time.After(time.Duration(2) * time.Minute):
		quit <- true
		log.Warning("Timed out waiting for AWS ELB to be ready.")
	}
}

func getAllDeployedNamespaces(deployment *apis.Deployment) []string {
	// Find all namespaces we deployed to
	allNamespaces := []string{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		newNamespace := ""
		if task.Deployment != nil {
			newNamespace = getNamespace(task.Deployment.ObjectMeta)
		} else if task.DaemonSet != nil {
			newNamespace = getNamespace(task.DaemonSet.ObjectMeta)
		}
		exists := false
		for _, namespace := range allNamespaces {
			if namespace == newNamespace {
				exists = true
				break
			}
		}

		if !exists {
			allNamespaces = append(allNamespaces, newNamespace)
		}
	}

	return allNamespaces
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

	for nodeName, id := range nodeInfos {
		if node, err := k8sClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{}); err == nil {
			node.Labels["hyperpilot/node-id"] = strconv.Itoa(id)
			node.Labels["hyperpilot/deployment"] = gcpCluster.Name
			if _, err := k8sClient.CoreV1().Nodes().Update(node); err == nil {
				log.Infof("Added label hyperpilot/node-id:%s to Kubernetes node %s", strconv.Itoa(id), nodeName)
			}
		} else {
			return fmt.Errorf("Unable to get Kubernetes node by name %s: %s", nodeName, err.Error())
		}
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

func getProjectId(serviceAccountPath string) (string, error) {
	viper := viper.New()
	viper.SetConfigType("json")
	viper.SetConfigFile(serviceAccountPath)
	err := viper.ReadInConfig()
	if err != nil {
		return "", err
	}
	return viper.GetString("project_id"), nil
}
