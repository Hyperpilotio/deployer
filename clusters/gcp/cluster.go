package gcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperpilotio/deployer/apis"
	"github.com/spf13/viper"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	storage "google.golang.org/api/storage/v1"
)

var defaultScopes = []string{
	container.CloudPlatformScope,
	compute.ComputeScope,
	storage.DevstorageFullControlScope,
}

type GCPProfile struct {
	UserId              string
	ServiceAccount      string
	ProjectId           string
	AuthJSONFileContent string
}

func (gcpProfile *GCPProfile) GetProjectId() (string, error) {
	if gcpProfile.ProjectId != "" {
		return gcpProfile.ProjectId, nil
	}

	jsonMap := make(map[string]interface{})
	if err := json.Unmarshal([]byte(gcpProfile.AuthJSONFileContent), &jsonMap); err != nil {
		return "", errors.New("Unable to get projectId: " + err.Error())
	}

	return jsonMap["project_id"].(string), nil
}

type GCPKeyPairOutput struct {
	KeyName    string
	PrivateKey *rsa.PrivateKey
	Pem        string
	Pub        string
}

type NodeInfo struct {
	Instance  *compute.Instance
	PublicIp  string
	PrivateIp string
}

// GCPCluster stores the data of a google cloud platform backed cluster
type GCPCluster struct {
	Zone           string
	Name           string
	ClusterId      string
	ClusterVersion string
	GCPProfile     *GCPProfile
	KeyPair        *GCPKeyPairOutput
	NodeInfos      map[int]*NodeInfo
	NodePoolIds    []string
}

func NewGCPCluster(
	config *viper.Viper,
	deployment *apis.Deployment) *GCPCluster {
	clusterId := CreateUniqueClusterId(deployment.Name)
	deployment.Name = clusterId
	gcpCluster := &GCPCluster{
		Name:        clusterId,
		Zone:        deployment.Region,
		ClusterId:   clusterId,
		NodeInfos:   make(map[int]*NodeInfo),
		NodePoolIds: make([]string, 0),
	}

	if deployment.KubernetesDeployment.GCPDefinition != nil {
		gcpCluster.ClusterVersion = deployment.KubernetesDeployment.GCPDefinition.ClusterVersion
	}

	return gcpCluster
}

func CreateClient(gcpProfile *GCPProfile) (*http.Client, error) {
	conf, err := google.JWTConfigFromJSON([]byte(gcpProfile.AuthJSONFileContent), defaultScopes...)
	if err != nil {
		return nil, errors.New("Unable to acquire generate config: " + err.Error())
	}

	return conf.Client(oauth2.NoContext), nil
}

func CreateKeypair(keyName string) (*GCPKeyPairOutput, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, errors.New("Unable to create private key: " + err.Error())
	}

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, errors.New("Unable to create public key: " + err.Error())
	}

	return &GCPKeyPairOutput{
		KeyName:    keyName,
		PrivateKey: privateKey,
		Pem: string(pem.EncodeToMemory(
			&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
			})),
		Pub: string(ssh.MarshalAuthorizedKey(publicKey)),
	}, nil
}

func (gcpCluster *GCPCluster) SshConfig(user string) (*ssh.ClientConfig, error) {
	privateKey := strings.Replace(gcpCluster.KeyPair.Pem, "\\n", "\n", -1)
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, errors.New("Unable to parse private key: " + err.Error())
	}

	clientConfig := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
	}

	return clientConfig, nil
}

// ReloadKeyPair reload KeyPair by keyName
func (gcpCluster *GCPCluster) ReloadKeyPair(keyMaterial string) error {
	gcpCluster.KeyPair = &GCPKeyPairOutput{
		KeyName: gcpCluster.KeyName(),
		Pem:     keyMaterial,
	}
	return nil
}

func (gcpCluster *GCPCluster) GetClusterType() string {
	return "GCP"
}

func (gcpCluster *GCPCluster) GetKeyMaterial() string {
	keyMaterial := ""
	if gcpCluster.KeyPair != nil {
		keyMaterial = gcpCluster.KeyPair.Pem
	}
	return keyMaterial
}

func (gcpCluster *GCPCluster) KeyName() string {
	return gcpCluster.Name + "-key"
}

func CreateUniqueClusterId(deploymentName string) string {
	timeSeq := strconv.FormatInt(time.Now().Unix(), 10)
	deploymentNames := strings.Split(deploymentName, "-")
	return fmt.Sprintf("%s-%s", strings.Join(deploymentNames[:len(deploymentNames)-1], "-"), timeSeq)
}
