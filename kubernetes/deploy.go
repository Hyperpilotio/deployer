package kubernetes

import (
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"

	"github.com/spf13/viper"
	//	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//	"k8s.io/client-go/kubernetes"
	//	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
)

// Ubuntu 16.04 with Docker
var k8sAmis = map[string]string{
	"us-west-1": "ami-1b1e4b7b",
	"us-west-2": "ami-3c4dec5c",
	"us-east-1": "ami-87705d90",
}

var masterInstallCommand = `sudo yum update -y && sudo yum install -y git &&
git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode && 
sudo mkdir -p /var/lib/kubelet &&
sudo mount --bind /var/lib/kubelet /var/lib/kubelet &&
sudo mount --make-shared /var/lib/kubelet &&
sed -i 's/\/var\/lib\/kubelet:\/var\/lib\/kubelet:shared/\/var\/lib\/kubelet:\/var\/lib\/kubelet/g' common.sh &&
sudo ./master.sh -n`

var agentInstallCommand = `sudo yum update -y && sudo yum install -y git &&
git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode && 
sudo mkdir -p /var/lib/kubelet &&
sudo mount --bind /var/lib/kubelet /var/lib/kubelet &&
sudo mount --make-shared /var/lib/kubelet &&
sed -i 's/\/var\/lib\/kubelet:\/var\/lib\/kubelet:shared/\/var\/lib\/kubelet:\/var\/lib\/kubelet/g' common.sh &&
sudo #MASTER_IP# ./worker.sh`

func waitUntilMasterReady() error {
	// Use client-go to poll kube master until it's ready.
	return nil
}

func installKubernetes(deployedCluster *awsecs.DeployedCluster) error {
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
		if i == 1 {
			command = masterInstallCommand
			masterPrivateIp = nodeInfo.PrivateIp
		} else {
			command = strings.Replace(agentInstallCommand, "#MASTER_IP#", masterPrivateIp, 1)
		}
		address := nodeInfo.PublicDnsName + ":22"
		sshClient := common.NewSshClient(address, &clientConfig)
		if err := sshClient.Connect(); err != nil {
			return errors.New("Unable to connect to server " + address + ": " + err.Error())
		}

		if err := sshClient.RunCommand(command); err != nil {
			return errors.New("Unable to run install command: " + err.Error())
		}

		if i == 1 {
			// Wait until master is ready before installing kubelets
			if err := waitUntilMasterReady(); err != nil {
				return errors.New("Unable to waitl for Master: " + err.Error())
			}
		}
	}

	return nil
}

func CreateDeployment(viper *viper.Viper, deployment *apis.Deployment, uploadedFiles map[string]string, deployedCluster *awsecs.DeployedCluster) error {
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

	return nil
}
