package kubernetes

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"

	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

const defaultNamespace string = "default"

// Ubuntu 16.04 with Docker
var k8sAmis = map[string]string{
	"us-west-1": "ami-1b1e4b7b",
	"us-west-2": "ami-3c4dec5c",
	"us-east-1": "ami-e87d8afe",
}

var kubeDeployCommand = `git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode`
var masterInstallCommand = kubeDeployCommand + ` && sudo ./master.sh`
var agentInstallCommand = kubeDeployCommand + ` && sudo MASTER_IP=#MASTER_IP# ./worker.sh`

func waitUntilMasterReady(publicDNSName string, timeout time.Duration) error {
	// Use client-go to poll kube master until it's ready.
	config := &rest.Config{
		Host: publicDNSName + ":8080",
	}

	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				if _, err := k8s.NewForConfig(config); err == nil {
					c <- true
				}
				time.Sleep(time.Second * 2)
			}
		}
	}()

	select {
	case <-c:
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for master to be ready")
	}
}

func retryConnectSSH(address string, config *ssh.ClientConfig) (*common.SshClient, error) {
	// maxRetries is the number of times a SSH connect.
	maxRetries := 5
	sshClient := common.NewSshClient(address, config)

	var sshError error
	for i := 1; i <= maxRetries; i++ {
		if err := sshClient.Connect(); err != nil {
			sshError = errors.New("Unable to connect to server: " + err.Error())
			glog.Infof("retryConnectSSH %d", i)
			time.Sleep(time.Duration(10) * time.Second)
		} else if err == nil {
			return &sshClient, nil
		}
	}

	if sshError == nil {
		return &sshClient, nil
	} else {
		return nil, sshError
	}
}

func installKubernetes(deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Install kubernetes on nodes...")
	// Install Kubernetes via ssh into each instance and run kube-deploy's
	// docker multi-node install script.
	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		}}

	glog.Info("Installing kubernetes master..")
	nodeInfo := deployedCluster.NodeInfos[1]
	masterPrivateIP := nodeInfo.PrivateIp
	address := nodeInfo.PublicDnsName + ":22"
	if sshClient, err := retryConnectSSH(address, &clientConfig); err != nil {
		return errors.New("Unable to connect to server " + address + ": " + err.Error())
	} else if sshClient != nil {
		if err := sshClient.RunCommand(masterInstallCommand, true); err != nil {
			return errors.New("Unable to run install command: " + err.Error())
		}
	}

	// Wait until master is ready before installing kubelets
	if err := waitUntilMasterReady(nodeInfo.PublicDnsName, time.Duration(30)*time.Minute); err != nil {
		return errors.New("Unable to wait for Master: " + err.Error())
	}

	glog.Info("Kubernetes master is ready")

	// key not sorted, key=1 is master node
	for i, nodeInfo := range deployedCluster.NodeInfos {
		if i != 1 {
			glog.Info("Installing kubernetes kubelet..")
			command := strings.Replace(agentInstallCommand, "#MASTER_IP#", masterPrivateIP, 1)
			address := nodeInfo.PublicDnsName + ":22"
			if sshClient, err := retryConnectSSH(address, &clientConfig); err != nil {
				return errors.New("Unable to connect to server " + address + ": " + err.Error())
			} else if sshClient != nil {
				if err := sshClient.RunCommand(command, true); err != nil {
					return errors.New("Unable to run install command: " + err.Error())
				}
			}
		}
	}

	return nil
}

// CreateDeployment start a deployment
func CreateDeployment(config *viper.Viper, deployment *apis.Deployment, uploadedFiles map[string]string, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")
	sess, sessionErr := awsecs.CreateSession(config, deployedCluster.Deployment, k8sAmis)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	iamSvc := iam.New(sess)

	if err := awsecs.SetupEC2Infra(config, deployment, uploadedFiles, ec2Svc, iamSvc, deployedCluster, k8sAmis); err != nil {
		return errors.New("Unable to setup ec2: " + err.Error())
	}

	if err := installKubernetes(deployedCluster); err != nil {
		//awsecs.DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to install kubernetes: " + err.Error())
	}

	if err := deployServices(deployment, deployedCluster); err != nil {
		DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// UpdateDeployment start a deployment on EC2 is ready
func UpdateDeployment(config *viper.Viper, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")

	if err := deployServices(deployment, deployedCluster); err != nil {
		DeleteDeployment(config, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func DeleteDeployment(config *viper.Viper, deployedCluster *awsecs.DeployedCluster) error {
	restConfig := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(restConfig); err == nil {
		deploys := c.Extensions().Deployments(defaultNamespace)
		if deployLists, listError := deploys.List(v1.ListOptions{}); listError == nil {
			for _, deployment := range deployLists.Items {
				deploys.Delete(deployment.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		replicaSets := c.Extensions().ReplicaSets(defaultNamespace)
		if replicaSetLists, listError := replicaSets.List(v1.ListOptions{}); listError == nil {
			for _, replicaSet := range replicaSetLists.Items {
				replicaSets.Delete(replicaSet.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		pods := c.CoreV1().Pods(defaultNamespace)
		if podLists, listError := pods.List(v1.ListOptions{}); listError == nil {
			for _, pod := range podLists.Items {
				pods.Delete(pod.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}

		services := c.CoreV1().Services(defaultNamespace)
		if serviceLists, listError := services.List(v1.ListOptions{}); listError == nil {
			for _, service := range serviceLists.Items {
				services.Delete(service.GetObjectMeta().GetName(), &v1.DeleteOptions{})
			}
		}
	}

	awsecs.DeleteDeployment(config, deployedCluster, k8sAmis)

	return nil
}

func deployServices(deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	config := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(config); err == nil {
		deploy := c.Extensions().Deployments(defaultNamespace)
		for _, task := range deployment.KubernetesDeployment.Kubernetes {
			deploySpec := task.Deployment
			family := task.Family

			// Assigning Pods to Nodes
			nodeSelector := map[string]string{}
			svcExternalName := ""
			if node, err := getNode(family, deployment, deployedCluster); err != nil {
				return errors.New("Unable to find node: " + err.Error())
			} else if node != nil {
				svcExternalName = node.PublicDnsName
				nodeSelector["kubernetes.io/hostname"] = node.PrivateIp
			}
			deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

			// Create service for each task
			service := c.CoreV1().Services(defaultNamespace)

			labels := map[string]string{"app": family}
			v1Service := &v1.Service{
				ObjectMeta: v1.ObjectMeta{
					Name:      family,
					Labels:    labels,
					Namespace: defaultNamespace,
				},
				Spec: v1.ServiceSpec{
					Type:         v1.ServiceTypeExternalName,
					ExternalName: svcExternalName,
				},
			}
			_, err = service.Create(v1Service)
			if err != nil {
				return fmt.Errorf("Unable to create service %s: %s", family, err)
			}
			glog.Infof("Created %s service", family)

			// start deploy deployment
			_, err = deploy.Create(&deploySpec)
			if err != nil {
				return fmt.Errorf("could not create deployment: %s", err)
			}
			glog.Infof("%s deployment created", family)
		}
	}

	return nil
}

func getNode(family string, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) (*awsecs.NodeInfo, error) {
	id := -1
	for _, node := range deployment.NodeMapping {
		if node.Task == family {
			id = node.Id
			break
		}
	}

	if id == -1 {
		return nil, errors.New("Unable to get node by family:" + family)
	}

	return deployedCluster.NodeInfos[id], nil
}
