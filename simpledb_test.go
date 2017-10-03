package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hyperpilotio/blobstore"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers"
	awsk8s "github.com/hyperpilotio/deployer/clustermanagers/awsk8s"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"

	"github.com/magiconair/properties/assert"
	"github.com/spf13/viper"
)

var config *viper.Viper
var deploymentStore blobstore.BlobStore
var profileStore blobstore.BlobStore
var k8sDeployer *awsk8s.K8SDeployer

const (
	TEST_USER_ID         = "alan"
	TEST_DEPLOYMENT_NAME = "tech-demo"
	TEST_BASTION_IP      = "1.1.1.1"
	TEST_MASTER_IP       = "2.2.2.2"
)

func init() {
	config = viper.New()
	config.SetConfigType("json")
	config.SetConfigFile("./documents/dev.config")
	config.ReadInConfig()

	deploymentStore, _ = blobstore.NewBlobStore("Deployments", config)
	profileStore, _ = blobstore.NewBlobStore("AWSProfiles", config)

	kubernetesDeployment := &apis.Deployment{
		UserId:               TEST_USER_ID,
		Name:                 TEST_DEPLOYMENT_NAME,
		Region:               "us-east-1",
		KubernetesDeployment: &apis.KubernetesDeployment{},
	}

	awsProfile := &hpaws.AWSProfile{
		UserId:    TEST_USER_ID,
		AwsId:     config.GetString("awsId"),
		AwsSecret: config.GetString("awsSecret"),
	}

	deployer, _ := clustermanagers.NewDeployer(config, awsProfile, "K8S", kubernetesDeployment, true)
	k8sDeployer = deployer.(*awsk8s.K8SDeployer)
	k8sDeployer.BastionIp = TEST_BASTION_IP
	k8sDeployer.MasterIp = TEST_MASTER_IP
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
		t.Errorf("Unable to store %s deployment status: %s", TEST_DEPLOYMENT_NAME, err.Error())
	}
}

func testLoadAllDeployments(t *testing.T) {
	deployments, err := deploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{
			ClusterManager: &awsk8s.StoreInfo{},
		}
	})

	if err != nil {
		t.Errorf("Unable to load deployment status: %s", err.Error())
	}

	for _, deployment := range deployments.([]interface{}) {
		storeDeployment := deployment.(*StoreDeployment)
		if storeDeployment.Name == TEST_DEPLOYMENT_NAME {
			assert.Equal(t, TEST_USER_ID, storeDeployment.UserId)
			assert.Equal(t, TEST_BASTION_IP, storeDeployment.ClusterManager.(awsk8s.StoreInfo).BastionIp)
			assert.Equal(t, TEST_MASTER_IP, storeDeployment.ClusterManager.(awsk8s.StoreInfo).MasterIp)
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
		if awsProfile.UserId == TEST_USER_ID {
			assert.Equal(t, awsProfile.AwsId, config.GetString("awsId"))
			assert.Equal(t, awsProfile.AwsSecret, config.GetString("awsSecret"))
		}
	}
}
