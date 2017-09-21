package kubernetes

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clusters"
	"github.com/hyperpilotio/deployer/common"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var publicPortType = 1

func DeployKubernetesObjects(
	config *viper.Viper,
	k8sClient *k8s.Clientset,
	deployment *apis.Deployment,
	log *logging.Logger) error {
	namespaces, namespacesErr := GetExistingNamespaces(k8sClient)
	if namespacesErr != nil {
		return errors.New("Unable to get existing namespaces: " + namespacesErr.Error())
	}

	if err := CreateSecrets(k8sClient, namespaces, deployment); err != nil {
		return errors.New("Unable to create secrets in k8s: " + err.Error())
	}

	if err := DeployServices(k8sClient, deployment, namespaces, log); err != nil {
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	// k8sDeployer.recordPublicEndpoints(k8sClient)

	return nil
}

func DeployServices(
	k8sClient *k8s.Clientset,
	deployment *apis.Deployment,
	existingNamespaces map[string]bool,
	log *logging.Logger) error {
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
		namespace := GetNamespace(deploySpec.ObjectMeta)
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
			err := CreateServiceForDeployment(namespace, family, k8sClient, task, container, log)
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
		namespace := GetNamespace(daemonSet.ObjectMeta)
		if err := CreateNamespaceIfNotExist(namespace, existingNamespaces, k8sClient); err != nil {
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

func CreateServiceForDeployment(
	namespace string,
	family string,
	k8sClient *k8s.Clientset,
	task apis.KubernetesTask,
	container v1.Container,
	log *logging.Logger) error {
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
			return fmt.Errorf("Unable to list daemonsets in namespace '%s' for deletion: ", namespace, listError.Error())
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
			return fmt.Errorf("Unable to list deployments in namespace '%s' for deletion: ", namespace, listError.Error())
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
			return fmt.Errorf("Unable to list replica sets in namespace '%s' for deletion: ", namespace, listError.Error())
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
			return fmt.Errorf("Unable to list services in namespace '%s' for deletion: %s", namespace, listError.Error())
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
			return fmt.Errorf("Unable to list pods in namespace '%s' for deletion: %s", namespace, listError.Error())
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
			return fmt.Errorf("Unable to list secrets in namespace '%s' for deletion: %s", namespace, listError.Error())
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

func (k8sDeployer *K8SDeployer) deployServices(k8sClient *k8s.Clientset, existingNamespaces map[string]bool) error {
	deployment := k8sDeployer.Deployment
	kubeConfig := k8sDeployer.KubeConfig
	log := k8sDeployer.DeploymentLog.Logger

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
		k8sDeployer.Services[family] = ServiceMapping{
			NodeId: mapping.Id,
		}

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

func (k8sDeployer *K8SDeployer) recordPublicEndpoints(k8sClient *k8s.Clientset) {
	deployment := k8sDeployer.Deployment
	log := k8sDeployer.DeploymentLog.Logger

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
							hostname := service.Status.LoadBalancer.Ingress[0].Hostname
							port := service.Spec.Ports[0].Port

							serviceMapping := ServiceMapping{
								PublicUrl: hostname + ":" + strconv.FormatInt(int64(port), 10),
							}

							familyName := serviceName[:strings.Index(serviceName, "-public")]
							if mapping, ok := k8sDeployer.Services[familyName]; ok {
								serviceMapping.NodeId = mapping.NodeId
							}
							k8sDeployer.Services[familyName] = serviceMapping
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

// DownloadKubeConfig is Use SSHProxyCommand download k8s master node's kubeconfig
func (k8sDeployer *K8SDeployer) DownloadKubeConfig() error {
	awsCluster := k8sDeployer.AWSCluster

	baseDir := awsCluster.Name + "_kubeconfig"
	basePath := "/tmp/" + baseDir
	kubeconfigFilePath := basePath + "/kubeconfig"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(kubeconfigFilePath)

	clientConfig, clientConfigErr := awsCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	sshClient := common.NewSshClient(k8sDeployer.MasterIp+":22", clientConfig, k8sDeployer.BastionIp+":22")
	if err := sshClient.CopyRemoteFileToLocal("/home/ubuntu/kubeconfig", kubeconfigFilePath); err != nil {
		return errors.New("Unable to copy kubeconfig file to local: " + err.Error())
	}

	k8sDeployer.KubeConfigPath = kubeconfigFilePath
	return nil
}

// UploadSshKeyToBastion upload sshKey to bastion-host
func (k8sDeployer *K8SDeployer) UploadSshKeyToBastion() error {
	awsCluster := k8sDeployer.AWSCluster

	baseDir := awsCluster.Name + "_sshkey"
	basePath := "/tmp/" + baseDir
	sshKeyFilePath := basePath + "/" + awsCluster.KeyName() + ".pem"

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		os.Mkdir(basePath, os.ModePerm)
	}

	os.Remove(sshKeyFilePath)

	privateKey := strings.Replace(*awsCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
	if err := ioutil.WriteFile(sshKeyFilePath, []byte(privateKey), 0400); err != nil {
		return fmt.Errorf("Unable to create %s sshKey file: %s",
			awsCluster.Name, err.Error())
	}

	clientConfig, err := awsCluster.SshConfig("ubuntu")
	if err != nil {
		return errors.New("Unable to create ssh config: " + err.Error())
	}

	address := k8sDeployer.BastionIp + ":22"
	scpClient := common.NewSshClient(address, clientConfig, "")

	remotePath := "/home/ubuntu/" + awsCluster.KeyName() + ".pem"
	if err := scpClient.CopyLocalFileToRemote(sshKeyFilePath, remotePath); err != nil {
		errorMsg := fmt.Sprintf("Unable to upload file %s to server %s: %s",
			awsCluster.KeyName(), address, err.Error())
		return errors.New(errorMsg)
	}

	return nil
}

func (k8sDeployer *K8SDeployer) GetCluster() clusters.Cluster {
	return k8sDeployer.AWSCluster
}

// CheckClusterState check kubernetes cluster state is exist
func (k8sDeployer *K8SDeployer) CheckClusterState() error {
	awsCluster := k8sDeployer.AWSCluster
	awsProfile := awsCluster.AWSProfile

	sess, sessionErr := hpaws.CreateSession(awsProfile, awsCluster.Region)
	if sessionErr != nil {
		return fmt.Errorf("Unable to create session: %s", sessionErr.Error())
	}

	cfSvc := cloudformation.New(sess)

	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(awsCluster.StackName()),
	}

	describeStacksOutput, err := cfSvc.DescribeStacks(describeStacksInput)
	if err != nil {
		return errors.New("Unable to get stack outputs: " + err.Error())
	}

	stackStatus := aws.StringValue(describeStacksOutput.Stacks[0].StackStatus)
	if stackStatus != "CREATE_COMPLETE" {
		return errors.New("Unable to reload stack because status is not ready, current status: " + stackStatus)
	}

	return nil
}

// ReloadClusterState reloads kubernetes cluster state
func (k8sDeployer *K8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	deploymentName := k8sDeployer.AWSCluster.Name
	if err := k8sDeployer.CheckClusterState(); err != nil {
		return fmt.Errorf("Skipping reloading because unable to load %s stack: %s", deploymentName, err.Error())
	}

	k8sStoreInfo := storeInfo.(*StoreInfo)
	k8sDeployer.BastionIp = k8sStoreInfo.BastionIp
	k8sDeployer.MasterIp = k8sStoreInfo.MasterIp
	k8sDeployer.VpcPeeringConnectionId = k8sStoreInfo.VpcPeeringConnectionId

	glog.Infof("Reloading kube config for %s...", k8sDeployer.AWSCluster.Name)
	if err := k8sDeployer.DownloadKubeConfig(); err != nil {
		return fmt.Errorf("Unable to download %s kubeconfig: %s", deploymentName, err.Error())
	}
	glog.Infof("Reloaded %s kube config at %s", k8sDeployer.AWSCluster.Name, k8sDeployer.KubeConfigPath)

	kubeConfig, err := clientcmd.BuildConfigFromFlags("", k8sDeployer.KubeConfigPath)
	if err != nil {
		return fmt.Errorf("Unable to parse %s kube config: %s", k8sDeployer.AWSCluster.Name, err.Error())
	}
	k8sDeployer.KubeConfig = kubeConfig

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}
	k8sDeployer.recordPublicEndpoints(k8sClient)

	return nil
}

func (k8sDeployer *K8SDeployer) GetClusterInfo() (*ClusterInfo, error) {
	kubeConfig := k8sDeployer.KubeConfig
	if kubeConfig == nil {
		return nil, errors.New("Empty kubeconfig passed, skipping to get k8s objects")
	}

	k8sClient, err := k8s.NewForConfig(kubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to kubernetes during get cluster: " + err.Error())
	}

	nodes := k8sClient.CoreV1().Nodes()
	nodeLists, nodeError := nodes.List(metav1.ListOptions{})
	if nodeError != nil {
		return nil, fmt.Errorf("Unable to list nodes for get cluster: %s", nodeError.Error())
	}

	pods := k8sClient.CoreV1().Pods("")
	podLists, podError := pods.List(metav1.ListOptions{})
	if podError != nil {
		return nil, fmt.Errorf("Unable to list pods for get cluster: %s", podError.Error())
	}

	deploys := k8sClient.Extensions().Deployments("")
	deployLists, depError := deploys.List(metav1.ListOptions{})
	if depError != nil {
		return nil, fmt.Errorf("Unable to list deployments for get cluster: %s", depError.Error())
	}

	clusterInfo := &ClusterInfo{
		Nodes:      nodeLists.Items,
		Pods:       podLists.Items,
		Containers: deployLists.Items,
		BastionIp:  k8sDeployer.BastionIp,
		MasterIp:   k8sDeployer.MasterIp,
	}

	return clusterInfo, nil
}

func (k8sDeployer *K8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	nodeNameInfos := map[string]string{}
	if len(k8sDeployer.AWSCluster.NodeInfos) > 0 {
		for id, nodeInfo := range k8sDeployer.AWSCluster.NodeInfos {
			nodeNameInfos[strconv.Itoa(id)] = aws.StringValue(nodeInfo.Instance.PrivateDnsName)
		}
	} else {
		k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
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
	for serviceName, serviceMapping := range k8sDeployer.Services {
		if serviceMapping.NodeId == 0 {
			serviceNodeId, err := findNodeIdFromServiceName(k8sDeployer.Deployment, serviceName)
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

// findNodeIdFromServiceName finds the node id that should be running this service
func findNodeIdFromServiceName(deployment *apis.Deployment, serviceName string) (int, error) {
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

// GetServiceAddress return ServiceAddress object
func (k8sDeployer *K8SDeployer) GetServiceAddress(serviceName string) (*apis.ServiceAddress, error) {
	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return nil, errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	services, err := k8sClient.CoreV1().Services("").List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("Unable to list services in the cluster: " + err.Error())
	}

	for _, service := range services.Items {
		if (service.ObjectMeta.Name == serviceName || service.ObjectMeta.Name == serviceName+"-publicport0") &&
			string(service.Spec.Type) == "LoadBalancer" {
			port := service.Spec.Ports[0].Port
			hostname := service.Status.LoadBalancer.Ingress[0].Hostname
			address := &apis.ServiceAddress{Host: hostname, Port: port}

			return address, nil
		}
	}

	return nil, errors.New("Service not found in endpoints")
}

func (k8sDeployer *K8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	if info, ok := k8sDeployer.Services[serviceName]; ok {
		return info.PublicUrl, nil
	}

	k8sClient, err := k8s.NewForConfig(k8sDeployer.KubeConfig)
	if err != nil {
		return "", errors.New("Unable to connect to Kubernetes during get service url: " + err.Error())
	}

	services, err := k8sClient.CoreV1().Services("").List(metav1.ListOptions{})
	if err != nil {
		return "", errors.New("Unable to list services in the cluster: " + err.Error())
	}

	for _, service := range services.Items {
		if (service.ObjectMeta.Name == serviceName || service.ObjectMeta.Name == serviceName+"-publicport0") &&
			string(service.Spec.Type) == "LoadBalancer" {
			nodeId, _ := findNodeIdFromServiceName(k8sDeployer.Deployment, serviceName)
			port := service.Spec.Ports[0].Port
			hostname := service.Status.LoadBalancer.Ingress[0].Hostname
			serviceUrl := hostname + ":" + strconv.FormatInt(int64(port), 10)
			k8sDeployer.Services[serviceName] = ServiceMapping{
				PublicUrl: serviceUrl,
				NodeId:    nodeId,
			}
			return serviceUrl, nil
		}
	}

	return "", errors.New("Service not found in endpoints")
}

func (k8sDeployer *K8SDeployer) GetStoreInfo() interface{} {
	return &StoreInfo{
		BastionIp:              k8sDeployer.BastionIp,
		MasterIp:               k8sDeployer.MasterIp,
		VpcPeeringConnectionId: k8sDeployer.VpcPeeringConnectionId,
	}
}

func (k8sDeployer *K8SDeployer) NewStoreInfo() interface{} {
	return &StoreInfo{}
}
