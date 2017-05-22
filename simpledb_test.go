package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/store"
	"github.com/magiconair/properties/assert"
	"github.com/spf13/viper"
)

var config *viper.Viper
var deploymentStore store.Store
var profileStore store.Store
var k8sDeployer *kubernetes.K8SDeployer

var testUserId = "alan"
var testDeploymentName = "tech-demo"
var testBastionIp = "1.1.1.1"
var testMasterIp = "2.2.2.2"

func init() {
	config = viper.New()
	config.SetConfigType("json")
	config.SetConfigFile("./documents/dev.config")
	config.ReadInConfig()

	deploymentStore, _ = store.NewStore("Deployments", config)
	profileStore, _ = store.NewStore("AWSProfiles", config)

	kubernetesDeployment := &apis.Deployment{
		UserId:               testUserId,
		Name:                 testDeploymentName,
		Region:               "us-east-1",
		KubernetesDeployment: &apis.KubernetesDeployment{},
	}

	awsProfile := &hpaws.AWSProfile{
		UserId:    testUserId,
		AwsId:     config.GetString("awsId"),
		AwsSecret: config.GetString("awsSecret"),
	}

	deployer, _ := clustermanagers.NewDeployer(config, awsProfile, "K8S", kubernetesDeployment, true)
	k8sDeployer = deployer.(*kubernetes.K8SDeployer)
	k8sDeployer.BastionIp = testBastionIp
	k8sDeployer.MasterIp = testMasterIp
}

func TestDeployments(t *testing.T) {
	t.Run("Store Deployments", testStoreDeployments)
	t.Run("Load Deployments", testLoadAllDeployments)
	t.Run("Delete Deployments", testDeleteDeployments)
}

func TestAWSProfiles(t *testing.T) {
	t.Run("Load Deployments", testLoadAllAWSProfiles)
}

func testStoreDeployments(t *testing.T) {
	b, err := json.Marshal(k8sDeployer.Deployment)
	if err != nil {
		t.Error("Unable to marshal deployment to json: " + err.Error())
	}

	storeDeployment := &StoreDeployment{
		Name:           k8sDeployer.Deployment.Name,
		UserId:         k8sDeployer.Deployment.UserId,
		Region:         k8sDeployer.Deployment.Region,
		Deployment:     string(b),
		Status:         "Creating",
		Created:        time.Now().Format(time.RFC822),
		Type:           "K8S",
		ClusterManager: k8sDeployer.GetStoreInfo(),
	}

	if err := deploymentStore.Store(storeDeployment.Name, storeDeployment); err != nil {
		t.Errorf("Unable to store %s deployment status: %s", testDeploymentName, err.Error())
	}
}

func testLoadAllDeployments(t *testing.T) {
	deployments, err := deploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{
			ClusterManager: &kubernetes.StoreInfo{},
		}
	})

	if err != nil {
		t.Errorf("Unable to load deployment status: %s", err.Error())
	}

	for _, deployment := range deployments.([]interface{}) {
		storeDeployment := deployment.(*StoreDeployment)
		if storeDeployment.Name == testDeploymentName {
			assert.Equal(t, testUserId, storeDeployment.UserId)
			assert.Equal(t, testBastionIp, storeDeployment.ClusterManager.(kubernetes.StoreInfo).BastionIp)
			assert.Equal(t, testMasterIp, storeDeployment.ClusterManager.(kubernetes.StoreInfo).MasterIp)
		}
	}
}

func testDeleteDeployments(t *testing.T) {
	if err := deploymentStore.Delete(k8sDeployer.Deployment.Name); err != nil {
		t.Errorf("Unable to delete deployment status: %s", err.Error())
	}
}

func testLoadAllAWSProfiles(t *testing.T) {
	profiles, err := profileStore.LoadAll(func() interface{} {
		return &hpaws.AWSProfile{}
	})

	if err != nil {
		t.Errorf("Unable to load awsProfile: %s", err.Error())
	}

	for _, profile := range profiles.([]interface{}) {
		awsProfile := profile.(*hpaws.AWSProfile)
		if awsProfile.UserId == testUserId {
			assert.Equal(t, awsProfile.AwsId, config.GetString("awsId"))
			assert.Equal(t, awsProfile.AwsSecret, config.GetString("awsSecret"))
		}
	}
}
