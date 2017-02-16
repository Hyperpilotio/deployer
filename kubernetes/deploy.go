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
	k8serrs "k8s.io/client-go/pkg/api/errors"
	"k8s.io/client-go/pkg/api/resource"
	"k8s.io/client-go/pkg/api/unversioned"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"
	"k8s.io/client-go/rest"
)

const namespace string = "default"

type deployOperation struct {
	image  string
	name   string
	port   int
	cpu    string
	memory string
}

type operation interface {
	Do(c *k8s.Clientset)
}

// Ubuntu 16.04 with Docker
var k8sAmis = map[string]string{
	"us-west-1": "ami-1b1e4b7b",
	"us-west-2": "ami-3c4dec5c",
	"us-east-1": "ami-6edd3078",
}

var kubedeployCommand = `sudo apt-get update -y && sudo apt-get install -y git &&
sudo apt-get install -y docker.io &&
git clone https://github.com/kubernetes/kube-deploy &&
cd kube-deploy/docker-multinode`

var masterInstallCommand = kubedeployCommand + ` && sudo ./master.sh`
var agentInstallCommand = kubedeployCommand + ` && sudo MASTER_IP=#MASTER_IP# ./worker.sh`

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
		//awsecs.DeleteDeployment(viper, deployedCluster)
		return errors.New("Unable to setup K8S: " + err.Error())
	}

	return nil
}

func setupK8S(deployment *apis.Deployment, deployedCluster *awsecs.DeployedCluster) error {
	publicDNSName := ""
	for i, nodeInfo := range deployedCluster.NodeInfos {
		if i == 1 {
			publicDNSName = nodeInfo.PublicDnsName
		}
	}

	config := &rest.Config{
		Host: publicDNSName + ":8080",
	}

	if c, err := k8s.NewForConfig(config); err == nil {
		// test deploy redis-bench
		// TODO range deployedCluster
		op := &deployOperation{
			image:  "hyperpilot/redis-bench",
			name:   "redis-bench",
			port:   6001,
			cpu:    "125.0m",
			memory: "150Mi",
		}
		op.Do(c)
	}

	return nil
}

func (op *deployOperation) Do(c *k8s.Clientset) error {
	if err := op.doDeployment(c); err != nil {
		return errors.New("Unable to deploy kubernetes deployment: " + err.Error())
	}

	if err := op.doService(c); err != nil {
		return errors.New("Unable to deploy kubernetes service: " + err.Error())
	}

	return nil
}

func (op *deployOperation) doDeployment(c *k8s.Clientset) error {
	// Define Deployments spec.
	appName := op.name
	deploySpec := &v1beta1.Deployment{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: v1.ObjectMeta{
			Name: appName,
		},
		Spec: v1beta1.DeploymentSpec{
			Replicas: int32p(1),
			Strategy: v1beta1.DeploymentStrategy{
				Type: v1beta1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &v1beta1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(0),
					},
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(1),
					},
				},
			},
			RevisionHistoryLimit: int32p(10),
			Template: v1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Name:   appName,
					Labels: map[string]string{"app": appName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							Name:  op.name,
							Image: op.image,
							Ports: []v1.ContainerPort{
								v1.ContainerPort{ContainerPort: int32(op.port), Protocol: v1.ProtocolTCP},
							},
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse(op.cpu),
									v1.ResourceMemory: resource.MustParse(op.memory),
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
					DNSPolicy:     v1.DNSClusterFirst,
				},
			},
		},
	}

	// Implement deployment update-or-create semantics.
	deploy := c.Extensions().Deployments(namespace)
	_, err := deploy.Update(deploySpec)
	switch {
	case err == nil:
		glog.Info("deployment controller updated")
	case !k8serrs.IsNotFound(err):
		return fmt.Errorf("could not update deployment controller: %s", err)
	default:
		_, err = deploy.Create(deploySpec)
		if err != nil {
			return fmt.Errorf("could not create deployment controller: %s", err)
		}
		glog.Info("deployment controller created")
	}

	return nil
}

func (op *deployOperation) doService(c *k8s.Clientset) error {
	// Define service spec.
	appName := op.name
	serviceSpec := &v1.Service{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: v1.ObjectMeta{
			Name: appName,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": appName},
			Ports: []v1.ServicePort{
				v1.ServicePort{
					Protocol: v1.ProtocolTCP,
					Port:     int32(op.port),
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(op.port),
					},
				},
			},
		},
	}

	// Implement service update-or-create semantics.
	service := c.Core().Services(namespace)
	svc, err := service.Get(appName)
	switch {
	case err == nil:
		serviceSpec.ObjectMeta.ResourceVersion = svc.ObjectMeta.ResourceVersion
		serviceSpec.Spec.ClusterIP = svc.Spec.ClusterIP
		_, err = service.Update(serviceSpec)
		if err != nil {
			return fmt.Errorf("failed to update service: %s", err)
		}
		glog.Info("service updated")
	case k8serrs.IsNotFound(err):
		_, err = service.Create(serviceSpec)
		if err != nil {
			return fmt.Errorf("failed to create service: %s", err)
		}
		glog.Info("service created")
	default:
		return fmt.Errorf("unexpected error: %s", err)
	}

	return nil
}

func int32p(i int32) *int32 {
	return &i
}
