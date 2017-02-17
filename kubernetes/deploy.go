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
	"k8s.io/client-go/rest"
)

const namespace string = "default"

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

func waitUntilMasterReady(publicDNSName string) error {
	// Use client-go to poll kube master until it's ready.
	masterReady := false
	config := &rest.Config{
		Host: publicDNSName + ":8080",
	}

	for !masterReady {
		if _, err := k8s.NewForConfig(config); err != nil {
			return errors.New("could not connect to Kubernetes API: " + err.Error())
		} else if err == nil {
			// TODO other condition
			break
		}
		time.Sleep(time.Duration(3) * time.Second)
	}
	glog.Info("Master reading...")

	return nil
}

func retryConnectSSH(address string, config *ssh.ClientConfig) (*common.SshClient, error) {
	retryTimes := 5
	sshClient := common.NewSshClient(address, config)
	for i := 1; i <= retryTimes; i++ {
		if err := sshClient.Connect(); err != nil {
			return nil, errors.New("Unable to connect to server " + address + ": " + err.Error())
		} else if err == nil {
			break
		}
		time.Sleep(time.Duration(5) * time.Second)
	}

	return &sshClient, nil
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
		if err := sshClient.RunCommand(masterInstallCommand); err != nil {
			return errors.New("Unable to run install command: " + err.Error())
		}
	}

	// Wait until master is ready before installing kubelets
	if err := waitUntilMasterReady(nodeInfo.PublicDnsName); err != nil {
		return errors.New("Unable to wait for Master: " + err.Error())
	}

	// key not sorted, key=1 is master node
	for i, nodeInfo := range deployedCluster.NodeInfos {
		if i != 1 {
			glog.Info("Installing kubernetes kubelet..")
			command := strings.Replace(agentInstallCommand, "#MASTER_IP#", masterPrivateIP, 1)
			address := nodeInfo.PublicDnsName + ":22"
			if sshClient, err := retryConnectSSH(address, &clientConfig); err != nil {
				return errors.New("Unable to connect to server " + address + ": " + err.Error())
			} else if sshClient != nil {
				if err := sshClient.RunCommand(command); err != nil {
					return errors.New("Unable to run install command: " + err.Error())
				}
			}
		}
	}

	return nil
}

// CreateDeployment start a deployment
func CreateDeployment(viper *viper.Viper, deployment *apis.Deployment, uploadedFiles map[string]string, deployedCluster *awsecs.DeployedCluster) error {
	glog.Info("Starting kubernetes deployment")
	sess, sessionErr := awsecs.CreateSession(viper, deployedCluster.Deployment, k8sAmis)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	iamSvc := iam.New(sess)

	if err := awsecs.SetupEC2Infra(viper, deployment, uploadedFiles, ec2Svc, iamSvc, deployedCluster, k8sAmis); err != nil {
		return errors.New("Unable to setup ec2: " + err.Error())
	}

	if err := installKubernetes(deployedCluster); err != nil {
		//awsecs.DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to install kubernetes: " + err.Error())
	}

	if err := setupK8S(deployment, deployedCluster); err != nil {
		// TODO delete client-go deployed
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func setupK8S(deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	config := &rest.Config{
		Host: deployedCluster.NodeInfos[1].PublicDnsName + ":8080",
	}

	if c, err := k8s.NewForConfig(config); err == nil {
		deploy := c.Extensions().Deployments(namespace)
		for _, Kubernetes := range deployment.KubernetesDeployment.Kubernetes {
			deploySpec := Kubernetes.Deployment
			family := Kubernetes.Family

			// Assigning Pods to Nodes
			nodeSelector := map[string]string{}
			if nodeIP, err := getNodeIP(family, deployment, deployedCluster); err != nil {
				return errors.New("Unable to find node ip: " + err.Error())
			} else if nodeIP != "" {
				nodeSelector["kubernetes.io/hostname"] = nodeIP
			}
			deploySpec.Spec.Template.Spec.NodeSelector = nodeSelector

			_, err = deploy.Create(&deploySpec)
			if err != nil {
				return fmt.Errorf("could not create deployment controller: %s", err)
			}
			glog.Info("deployment controller created")
		}
	}

	return nil
}

func getNodeIP(family string, deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) (string, error) {
	id := -1
	privateIP := ""
	for _, node := range deployment.NodeMapping {
		if node.Task == family {
			id = node.Id
			break
		}
	}

	if id == -1 {
		return privateIP, errors.New("Unable to get node id by family:" + family)
	}

	return deployedCluster.NodeInfos[id].PrivateIp, nil
}
