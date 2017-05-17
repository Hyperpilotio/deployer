package awsecs

import (
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/log"
	logging "github.com/op/go-logging"
	"github.com/spf13/viper"
)

type DeploymentState int

// Possible deployment states
const (
	AVAILABLE = 0
	CREATING  = 1
	UPDATING  = 2
	DELETING  = 3
	DELETED   = 4
	FAILED    = 5
)

type DeploymentInfo struct {
	AwsProfile *AWSProfile
	AwsInfo    *DeployedCluster
	K8sInfo    *KubernetesDeployment
	Created    time.Time
	State      DeploymentState
}

type AWSProfile struct {
	UserId    string
	AwsId     string
	AwsSecret string
}

type KubernetesDeployment struct {
	BastionIp      string
	MasterIp       string
	KubeConfigPath string
	Endpoints      map[string]string
	KubeConfig     *rest.Config
}

type NodeInfo struct {
	Instance      *ec2.Instance
	Arn           string
	PublicDnsName string
	PrivateIp     string
}

// DeployedCluster stores the data of a cluster
type DeployedCluster struct {
	Name              string
	KeyPair           *ec2.CreateKeyPairOutput
	Deployment        *apis.Deployment
	Logger            *logging.Logger
	SecurityGroupId   string
	SubnetId          string
	InternetGatewayId string
	NodeInfos         map[int]*NodeInfo
	InstanceIds       []*string
	VpcId             string
}

type ECSDeployer struct {
	Config *viper.Viper

	DeploymentInfo *DeploymentInfo
	DeploymentLog  *log.DeploymentLog
	Scheduler      *job.Scheduler

	mutex sync.Mutex
}

type StoreDeployment struct {
	Name        string
	Region      string
	Type        string
	Status      string
	Created     string
	KeyMaterial string
	UserId      string

	K8SDeployment *K8SStoreDeployment
}

type K8SStoreDeployment struct {
	BastionIp string
	MasterIp  string
}

type ClusterInfo struct {
	ContainerInstances []*ecs.ContainerInstance
	Reservations       []*ec2.Reservation
	Tasks              []*ecs.Task
}

func (deploymentInfo *DeploymentInfo) GetDeploymentType() string {
	if deploymentInfo.AwsInfo.Deployment.KubernetesDeployment != nil {
		return "K8S"
	} else {
		return "ECS"
	}
}

func GetStateString(state DeploymentState) string {
	switch state {
	case AVAILABLE:
		return "Available"
	case CREATING:
		return "Creating"
	case UPDATING:
		return "Updating"
	case DELETING:
		return "Deleting"
	case DELETED:
		return "Deleted"
	case FAILED:
		return "Failed"
	}

	return ""
}
