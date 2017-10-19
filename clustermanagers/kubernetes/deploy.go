package kubernetes

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/go-utils/funcs"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
	"k8s.io/client-go/rest"
)

var publicPortType = 1

func DeployKubernetesObjects(
	config *viper.Viper,
	k8sClient *k8s.Clientset,
	deployment *apis.Deployment,
	userName string,
	log *logging.Logger) error {
	namespaces, namespacesErr := GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := CreateSecrets(k8sClient, namespaces, deployment); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := DeployServices(config, k8sClient, deployment, "", namespaces, userName, log); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}
	deployClusterRoleAndBindings(k8sClient, log)

	return nil
}

func DeployServices(
	config *viper.Viper,
	k8sClient *k8s.Clientset,
	deployment *apis.Deployment,
	deployNamespace string,
	existingNamespaces map[string]bool,
	userName string,
	log *logging.Logger) error {
	tasks := map[string]apis.KubernetesTask{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		tasks[task.Family] = task
	}

	taskCount := map[string]int{}

	// We sort before we create services because we want to have a deterministic way to assign
	// service ids
	sort.Sort(deployment.NodeMapping)

	skipCreatePublicService := false
	if deployment.ClusterType == "GCP" || config.GetBool("inCluster") {
		skipCreatePublicService = true
	}

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

		namespace := GetNamespace(deploySpec.ObjectMeta)
		if deployNamespace != "" {
			namespace = deployNamespace
		}
		if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
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
		taskCount[originalFamily] = count

		// Assigning Pods to Nodes
		nodeSelector := map[string]string{}
		log.Infof("Selecting node %d for deployment %s", mapping.Id, family)
		nodeSelector["hyperpilot/node-id"] = strconv.Itoa(mapping.Id)
		nodeSelector["hyperpilot/deployment"] = deployment.Name

		deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

		// Create service for each container that opens a port
		for _, container := range deploySpec.Spec.Template.Spec.Containers {
			err := CreateServiceForDeployment(namespace, family, k8sClient,
				task, container, log, skipCreatePublicService, false)
			if err != nil {
				return fmt.Errorf("Unable to create service for deployment %s: %s", family, err.Error())
			}
		}

		for _, mount := range deploySpec.Spec.Template.Spec.Volumes {
			if mount.HostPath != nil && strings.HasPrefix(mount.HostPath.Path, "~/") {
				mount.HostPath.Path = strings.Replace(mount.HostPath.Path, "~/", "/home/"+userName+"/", 1)
			}
		}

		deploy := k8sClient.Extensions().Deployments(namespace)
		_, err := deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("Unable to create k8s deployment: %s", err)
		}
		log.Infof("%s deployment created", family)
	}

	if deployNamespace != "" {
		if err := checkDeploymentReadyReplicas(config, k8sClient, deployment, deployNamespace); err != nil {
			return fmt.Errorf("Unable to check deployment ready replicas status: %s", err)
		}
	}

	// Run daemonsets
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.DaemonSet == nil {
			continue
		}

		daemonSet := task.DaemonSet
		namespace := GetNamespace(daemonSet.ObjectMeta)
		if deployNamespace != "" {
			namespace = deployNamespace
		}
		if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		daemonSets := k8sClient.Extensions().DaemonSets(namespace)
		log.Infof("Creating daemonset %s", task.Family)
		if _, err := daemonSets.Create(daemonSet); err != nil {
			return fmt.Errorf("Unable to create daemonset %s: %s", task.Family, err.Error())
		}
	}

	// Run statefulsets
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		if task.StatefulSet == nil {
			continue
		}

		statefulSet := task.StatefulSet
		namespace := GetNamespace(statefulSet.ObjectMeta)
		if deployNamespace != "" {
			namespace = deployNamespace
		}
		if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return err
		}

		statefulSets := k8sClient.StatefulSets(namespace)
		log.Infof("Creating statefulset %s", task.Family)
		if _, err := statefulSets.Create(statefulSet); err != nil {
			return fmt.Errorf("Unable to create statefulset %s: %s", task.Family, err.Error())
		}

		for _, container := range task.StatefulSet.Spec.Template.Spec.Containers {
			if err := CreateServiceForDeployment(namespace, task.Family, k8sClient, task, container, log, false, true); err != nil {
				return fmt.Errorf("Unable to create service for stateful set: " + err.Error())
			}
		}
	}

	return nil
}

func checkDeploymentReadyReplicas(
	config *viper.Viper,
	k8sClient *k8s.Clientset,
	deployment *apis.Deployment,
	namespace string) error {
	restartCount := config.GetInt("restartCount")
	deploy := k8sClient.Extensions().Deployments(namespace)
	err := funcs.LoopUntil(time.Minute*60, time.Second*20, func() (bool, error) {
		deployments, listErr := deploy.List(metav1.ListOptions{})
		if listErr != nil {
			return false, errors.New("Unable to list deployments: " + listErr.Error())
		}

		if len(deployments.Items) != len(deployment.NodeMapping) {
			return false, fmt.Errorf("Unexpected list of deployments: %d", len(deployments.Items))
		}

		pods, listErr := k8sClient.CoreV1().Pods(namespace).List(metav1.ListOptions{})
		if listErr != nil {
			return false, errors.New("Unable to list pods: " + listErr.Error())
		}
		for _, pod := range pods.Items {
			switch pod.Status.Phase {
			case "Pending":
				for _, condition := range pod.Status.Conditions {
					if condition.Reason == "Unschedulable" {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, condition.Message)
					}
				}
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.State.Waiting.Reason == "ImagePullBackOff" {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, containerStatus.State.Waiting.Message)
					}
				}
			case "Running":
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.RestartCount >= int32(restartCount) {
						return false, fmt.Errorf("Unable to create %s deployment: %s",
							pod.Name, containerStatus.State.Waiting.Message)
					}
				}
			}
		}

		for _, deployment := range deployments.Items {
			if deployment.Status.ReadyReplicas == 0 {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf("Unable to wait for deployments to be available: %s", err.Error())
	}

	return nil
}

func deployClusterRoleAndBindings(k8sClient *k8s.Clientset, log *logging.Logger) {
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
}

func CreateNamespaceIfNotExist(
	namespace string,
	existingNamespaces map[string]bool,
	k8sClient *k8s.Clientset) error {
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

func CreateSecrets(
	k8sClient *k8s.Clientset,
	existingNamespaces map[string]bool,
	deployment *apis.Deployment) error {
	secrets := deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	for _, secret := range secrets {
		namespace := GetNamespace(secret.ObjectMeta)
		if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
			return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
		}

		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func CreateSecretsByNamespace(
	k8sClient *k8s.Clientset,
	namespace string,
	deployment *apis.Deployment) error {
	secrets := deployment.KubernetesDeployment.Secrets
	if len(secrets) == 0 {
		return nil
	}

	existingNamespaces, namespacesErr := GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
		return fmt.Errorf("Unable to create namespace %s: %s", namespace, err.Error())
	}

	for _, secret := range secrets {
		k8sSecret := k8sClient.CoreV1().Secrets(namespace)
		if _, err := k8sSecret.Create(&secret); err != nil {
			return fmt.Errorf("Unable to create secret %s: %s", secret.Name, err.Error())
		}
	}

	return nil
}

func CreateServiceForDeployment(
	namespace string,
	family string,
	k8sClient *k8s.Clientset,
	task apis.KubernetesTask,
	container v1.Container,
	log *logging.Logger,
	skipCreatePublicService bool,
	internalHeadlessService bool) error {
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

	clusterIp := ""
	if internalHeadlessService {
		clusterIp = api.ClusterIPNone
	}

	internalService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Labels:    labels,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type:      v1.ServiceTypeClusterIP,
			ClusterIP: clusterIp,
			Ports:     servicePorts,
			Selector:  labels,
		},
	}
	_, err := service.Create(internalService)
	if err != nil {
		return fmt.Errorf("Unable to create service %s: %s", serviceName, err)
	}
	log.Infof("Created %s internal service", serviceName)

	// Check the type of each port opened by the container; create a loadbalancer service to expose the public port
	if skipCreatePublicService || task.PortTypes == nil || len(task.PortTypes) == 0 {
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

func DeleteK8S(namespaces []string, kubeConfig *rest.Config, log *logging.Logger) error {
	if kubeConfig == nil {
		return errors.New("Empty kubeconfig passed, skipping to delete k8s objects")
	}

	log.Info("Found kube config, deleting kubernetes objects")
	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during delete: " + err.Error())
	}

	for _, namespace := range namespaces {
		log.Info("Deleting kubernetes objects in namespace " + namespace)
		daemonsets := k8sClient.Extensions().DaemonSets(namespace)
		if daemonsetList, listError := daemonsets.List(metav1.ListOptions{}); listError == nil {
			for _, daemonset := range daemonsetList.Items {
				name := daemonset.GetObjectMeta().GetName()
				if err := daemonsets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete daemonset %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list daemonsets in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		deploys := k8sClient.Extensions().Deployments(namespace)
		if deployLists, listError := deploys.List(metav1.ListOptions{}); listError == nil {
			for _, deployment := range deployLists.Items {
				name := deployment.GetObjectMeta().GetName()
				if err := deploys.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete deployment %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list deployments in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		replicaSets := k8sClient.Extensions().ReplicaSets(namespace)
		if replicaSetList, listError := replicaSets.List(metav1.ListOptions{}); listError == nil {
			for _, replicaSet := range replicaSetList.Items {
				name := replicaSet.GetObjectMeta().GetName()
				if err := replicaSets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					glog.Warningf("Unable to delete replica set %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list replica sets in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		services := k8sClient.CoreV1().Services(namespace)
		if serviceLists, listError := services.List(metav1.ListOptions{}); listError == nil {
			for _, service := range serviceLists.Items {
				serviceName := service.GetObjectMeta().GetName()
				if err := services.Delete(serviceName, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", serviceName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list services in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		pods := k8sClient.CoreV1().Pods(namespace)
		if podLists, listError := pods.List(metav1.ListOptions{}); listError == nil {
			for _, pod := range podLists.Items {
				podName := pod.GetObjectMeta().GetName()
				if err := pods.Delete(podName, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete pod %s: %s", podName, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list pods in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		secrets := k8sClient.CoreV1().Secrets(namespace)
		if secretList, listError := secrets.List(metav1.ListOptions{}); listError == nil {
			for _, secret := range secretList.Items {
				name := secret.GetObjectMeta().GetName()
				if err := secrets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list secrets in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}

		statefulSets := k8sClient.StatefulSets(namespace)
		if statefulSetsList, listError := statefulSets.List(metav1.ListOptions{}); listError == nil {
			for _, statefulSet := range statefulSetsList.Items {
				name := statefulSet.GetObjectMeta().GetName()
				if err := statefulSets.Delete(name, &metav1.DeleteOptions{}); err != nil {
					log.Warningf("Unable to delete service %s: %s", name, err.Error())
				}
			}
		} else {
			return fmt.Errorf("Unable to list statefulSets in namespace '%s' for deletion: \n%s",
				namespace, listError.Error())
		}
	}

	return nil
}

func GetExistingNamespaces(k8sClient *k8s.Clientset) (map[string]bool, error) {
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

func GetAllDeployedNamespaces(deployment *apis.Deployment) []string {
	// Find all namespaces we deployed to
	allNamespaces := []string{}
	for _, task := range deployment.KubernetesDeployment.Kubernetes {
		newNamespace := ""
		if task.Deployment != nil {
			newNamespace = GetNamespace(task.Deployment.ObjectMeta)
		} else if task.DaemonSet != nil {
			newNamespace = GetNamespace(task.DaemonSet.ObjectMeta)
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

func GetNamespace(objectMeta metav1.ObjectMeta) string {
	namespace := objectMeta.Namespace
	if namespace == "" {
		return "default"
	}

	return namespace
}

func TagKubeNodes(
	k8sClient *k8s.Clientset,
	deploymentName string,
	nodeInfos map[string]int,
	log *logging.Logger) error {
	for nodeName, id := range nodeInfos {
		if node, err := k8sClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{}); err == nil {
			node.Labels["hyperpilot/node-id"] = strconv.Itoa(id)
			node.Labels["hyperpilot/deployment"] = deploymentName
			if _, err := k8sClient.CoreV1().Nodes().Update(node); err == nil {
				log.Infof("Added label hyperpilot/node-id:%s to Kubernetes node %s", strconv.Itoa(id), nodeName)
				log.Infof("Added label hyperpilot/deployment:%s to Kubernetes node %s", deploymentName, nodeName)
			}
		} else {
			return fmt.Errorf("Unable to get Kubernetes node by name %s: %s", nodeName, err.Error())
		}
	}

	return nil
}

// FindNodeIdFromServiceName finds the node id that should be running this service
func FindNodeIdFromServiceName(deployment *apis.Deployment, serviceName string) (int, error) {
	// if a service name contains a number (e.g: benchmark-agent-2), we assume
	// it's the second benchmark agent from the mapping. Since we should be sorting
	// the node ids when we deploy them, we should always assign the same service name
	// for the same app running on the same node.
	parts := strings.Split(serviceName, "-")
	count := 1
	realServiceName := serviceName
	if len(parts) > 0 {
		if nth, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			count = nth
			realServiceName = strings.Join(parts[:len(parts)-1], "-")
		}
	}
	sort.Sort(deployment.NodeMapping)
	current := 0
	for _, mapping := range deployment.NodeMapping {
		if mapping.Task == realServiceName {
			current += 1
			if current == count {
				return mapping.Id, nil
			}
		}
	}

	return 0, errors.New("Unable to find service in mappings")
}

func DeleteNodeReaderClusterRoleBindingToNamespace(k8sClient *k8s.Clientset, namespace string, log *logging.Logger) {
	roleName := "node-reader"
	roleBinding := roleName + "-binding-" + namespace

	log.Infof("Deleting cluster role binding %s for namespace %s", roleBinding, namespace)
	clusterRoleBinding := k8sClient.RbacV1beta1().ClusterRoleBindings()
	if err := clusterRoleBinding.Delete(roleBinding, &metav1.DeleteOptions{}); err != nil {
		log.Warningf("Unable to delete cluster role binding %s for namespace %s", roleName, roleBinding)
	}
	log.Infof("Successfully delete cluster role binding %s", roleBinding)
}

// GrantNodeReaderPermissionToNamespace grant read permission to the default user belongs to specified namespace
func GrantNodeReaderPermissionToNamespace(k8sClient *k8s.Clientset, namespace string, log *logging.Logger) error {
	roleName := "node-reader"
	roleBinding := roleName + "-binding-" + namespace

	// check whether role-binding exists
	clusterRoleBindings := k8sClient.RbacV1beta1().ClusterRoleBindings()
	clusterRoleBindingList, err := clusterRoleBindings.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("Unable to list role-binding in cluster scope: %s", err.Error())
	}
	for _, val := range clusterRoleBindingList.Items {
		if val.GetName() == roleBinding {
			log.Infof("Found role binding %s in cluster scope", roleBinding)
			return nil
		}
	}

	// check whether role exists or not
	clusterRoles := k8sClient.RbacV1beta1().ClusterRoles()
	roleList, err := clusterRoles.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf(`Unable to list roles in cluster scope: %s`, err.Error())
	}
	var roleExists bool
	for _, val := range roleList.Items {
		if val.GetName() == roleName {
			roleExists = true
		}
	}
	if !roleExists {
		return fmt.Errorf("Role %s doesn't exist in cluster scope", roleName)
	}

	log.Infof("Binding cluster role '%s' to namespace %s", roleName, namespace)
	roleBindingObject := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleBinding,
		},
		Subjects: []rbac.Subject{
			rbac.Subject{
				Kind:      rbac.ServiceAccountKind,
				Name:      "default",
				Namespace: namespace,
			},
		},
		RoleRef: rbac.RoleRef{
			APIGroup: "",
			Kind:     "ClusterRole",
			Name:     roleName,
		},
	}

	if _, err = clusterRoleBindings.Create(roleBindingObject); err != nil {
		return fmt.Errorf("Unable to create role binding for namespace %s in cluster scope: %s", namespace, err.Error())
	}
	log.Infof("Successfully binds cluster role '%s' to namespace '%s'", roleName, namespace)

	return nil
}

func WaitUntilKubernetesNodeExists(
	k8sClient *k8s.Clientset,
	nodeNames []string,
	timeout time.Duration,
	log *logging.Logger) error {
	return funcs.LoopUntil(timeout, time.Second*10, func() (bool, error) {
		allExists := true
		for _, nodeName := range nodeNames {
			if _, err := k8sClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{}); err != nil {
				allExists = false
				break
			}
		}
		if allExists {
			log.Infof("Kubernetes nodes are available now")
			return true, nil
		}
		return false, nil
	})
}
