package gcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
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
)

type GCPProfile struct {
	ServiceAccount   string
	AuthJSONFilePath string
	ProjectId        string
	Scopes           []string
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
}

func NewGCPCluster(config *viper.Viper, deployment *apis.Deployment) *GCPCluster {
	clusterId := CreateUniqueClusterId(deployment.Name)
	return &GCPCluster{
		Zone:           deployment.Region,
		Name:           deployment.Name,
		ClusterId:      clusterId,
		ClusterVersion: deployment.KubernetesDeployment.GCPDefinition.ClusterVersion,
		GCPProfile: &GCPProfile{
			ServiceAccount: deployment.KubernetesDeployment.GCPDefinition.ServiceAccount,
			Scopes: []string{
				container.CloudPlatformScope,
				compute.ComputeScope,
			},
			AuthJSONFilePath: config.GetString("gpcServiceAccountJSONFile"),
		},
		NodeInfos: make(map[int]*NodeInfo),
	}
}

func CreateClient(gcpProfile *GCPProfile, Zone string) (*http.Client, error) {
	dat, err := ioutil.ReadFile(gcpProfile.AuthJSONFilePath)
	if err != nil {
		return nil, errors.New("Unable to read service account file: " + err.Error())
	}

	conf, err := google.JWTConfigFromJSON(dat, gcpProfile.Scopes...)
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

func (gcpCluster *GCPCluster) GetClusterType() string {
	return "GCP"
}

func (gcpCluster *GCPCluster) GetKeyMaterial() string {
	return gcpCluster.KeyPair.Pem
}

func (gcpCluster *GCPCluster) KeyName() string {
	return gcpCluster.Name + "-key"
}

func CreateUniqueClusterId(deploymentName string) string {
	timeSeq := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("%s-%s", strings.Split(deploymentName, "-")[0], timeSeq)
}
