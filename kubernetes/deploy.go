package kubernetes

import (
	"strings"

	"github.com/hyperpilotio/deployer/awsecs"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Ubuntu 16.04 with Docker
var k8sAmis = map[string]string{
	"us-west-1": "ami-1b1e4b7b",
	"us-west-2": "ami-3c4dec5c",
	"us-east-1": "ami-87705d90",
}

var masterInstallCommand = `git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode && sudo ./master.sh`

var agentInstallCommand = `git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode && sudo #MASTER_IP# ./worker.sh`

func waitUntilMasterReady() error {
	// Use client-go to poll kube master until it's ready.
}

func installKubernetes(deployedCluster *DeployedCluster) error {
	// Install Kubernetes via ssh into each instance and run kube-deploy's
	// docker multi-node install script.
	privateKey := strings.Replace(*deployedCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := ssh.ClientConfig{
		User: "ec2-user",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		}}

	masterPrivateIp := ""
	for i, nodeInfo := range deployedCluster.NodeInfos {
		command := ""
		if i == 0 {
			command = masterInstallCommand
			masterPrivateIp = nodeInfo.PrivateIp
		} else {
			command = strings.Replace(agentInstallCommand, "#MASTER_IP#", masterPrivateIp)
		}
		address := nodeInfo.PublicDnsName + ":22"
		sshClient := common.NewSshClient(address, &clientConfig)
		if err := sshClient.RunCommand(command); err != nil {
			return errors.New("Unable to run install command: " + err.Error())
		}

		if i == 0 {
			// Wait until master is ready before installing kubelets
			if err := waitUntilMasterReady(); err != nil {
				return errors.New("Unable to waitl for Master: " + err.Error())
			}
		}
	}

	return nil
}

func CreateDeployment(viper *viper.Viper, deployment *apis.Deployment, uploadedFiles map[string]string, deployedCluster *DeployedCluster) error {
	sess, sessionErr := awsecs.CreateSession(viper, deployedCluster.Deployment)
	if sessionErr != nil {
		return errors.New("Unable to create session: " + sessionErr.Error())
	}

	ec2Svc := ec2.New(sess)
	iamSvc := iam.New(sess)

	if err := awsecs.SetupEC2Infra(viper, deployment, uploadedFiles, ec2Svc, iamSvc, deployedCluster); err != nil {
		return errors.New("Unable to setup ec2: " + err.Error())
	}

	if err := installKubernetes(deployedCluster); err != nil {
		return errors.New("Unable to install kubernetes: " + err.Error())
	}
}
