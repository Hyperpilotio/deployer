package kubernetes

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/spf13/viper"

	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/log"
	"k8s.io/client-go/rest"
)

type InClusterK8SDeployer struct {
	K8SDeployer

	OriginalAWSCluster *hpaws.AWSCluster
}

func NewInClusterDeployer(
	config *viper.Viper,
	awsProfile *hpaws.AWSProfile,
	deployment *apis.Deployment,
	originalAWSCluster *hpaws.AWSCluster,
	kubeConfig *rest.Config) (*InClusterK8SDeployer, error) {
	log, err := log.NewLogger(config.GetString("filesPath"), deployment.Name)
	if err != nil {
		return nil, errors.New("Error creating deployment logger: " + err.Error())
	}

	awsCluster := hpaws.NewAWSCluster(deployment, awsProfile)
	deployer := &InClusterK8SDeployer{
		K8SDeployer: K8SDeployer{
			Config:        config,
			AWSCluster:    awsCluster,
			Deployment:    deployment,
			DeploymentLog: log,
			Services:      make(map[string]ServiceMapping),
			KubeConfig:    kubeConfig,
		},
		OriginalAWSCluster: originalAWSCluster,
	}

	return deployer, nil
}

// CreateDeployment start a deployment
func (inK8SDeployer *InClusterK8SDeployer) CreateDeployment(uploadedFiles map[string]string) (interface{}, error) {
	awsCluster := inK8SDeployer.OriginalAWSCluster
	log := inK8SDeployer.DeploymentLog.Logger

	sess, sessionErr := hpaws.CreateSession(awsCluster.AWSProfile, awsCluster.Region)
	if sessionErr != nil {
		return nil, fmt.Errorf("Unable to create session: %s" + sessionErr.Error())
	}

	autoscalingSvc := autoscaling.New(sess)

	result, err := autoscalingSvc.DescribeAutoScalingGroups(nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to describe auto scaling groups: %s" + err.Error())
	}

	launchConfigurationName := ""
	autoScalingGroupName := ""
	for _, group := range result.AutoScalingGroups {
		for _, tag := range group.Tags {
			if *tag.Key == "KubernetesCluster" && *tag.Value == awsCluster.StackName() {
				launchConfigurationName = *group.LaunchConfigurationName
				autoScalingGroupName = *group.AutoScalingGroupName
				break
			}
		}
	}

	log.Infof("launchConfigurationName: %s", launchConfigurationName)
	log.Infof("autoScalingGroupName: %s", autoScalingGroupName)

	return nil, nil
}

// UpdateDeployment start a deployment on EC2 is ready
func (inK8SDeployer *InClusterK8SDeployer) UpdateDeployment() error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) DeployExtensions(
	extensions *apis.Deployment,
	newDeployment *apis.Deployment) error {
	return nil
}

// DeleteDeployment clean up the cluster from kubenetes.
func (inK8SDeployer *InClusterK8SDeployer) DeleteDeployment() error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) ReloadClusterState(storeInfo interface{}) error {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetStoreInfo() interface{} {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetAWSCluster() *hpaws.AWSCluster {
	return inK8SDeployer.AWSCluster
}

func (inK8SDeployer *InClusterK8SDeployer) GetLog() *log.FileLog {
	return inK8SDeployer.DeploymentLog
}

func (inK8SDeployer *InClusterK8SDeployer) GetScheduler() *job.Scheduler {
	return nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetServiceUrl(serviceName string) (string, error) {
	return "", nil
}

func (inK8SDeployer *InClusterK8SDeployer) GetServiceMappings() (map[string]interface{}, error) {
	return nil, nil
}
