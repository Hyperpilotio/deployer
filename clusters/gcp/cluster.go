package gcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

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
	UserId           string
	ServiceAccount   string
	ProjectId        string
	AuthJSONFilePath string
}

func (gcpProfile *GCPProfile) GetProjectId() (string, error) {
	if gcpProfile.ProjectId != "" {
		return gcpProfile.ProjectId, nil
	}

	viper := viper.New()
	viper.SetConfigType("json")
	viper.SetConfigFile(gcpProfile.AuthJSONFilePath)
	err := viper.ReadInConfig()
	if err != nil {
		return "", err
	}

	return viper.GetString("project_id"), nil
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

func NewGCPCluster(
	config *viper.Viper,
	deployment *apis.Deployment) *GCPCluster {
	clusterId := CreateUniqueClusterId(deployment.Name)
	deployment.Name = clusterId
	gcpCluster := &GCPCluster{
		Zone:      deployment.Region,
		ClusterId: clusterId,
		NodeInfos: make(map[int]*NodeInfo),
	}

	if deployment.KubernetesDeployment.GCPDefinition != nil {
		gcpCluster.ClusterVersion = deployment.KubernetesDeployment.GCPDefinition.ClusterVersion
	}

	return gcpCluster
}

func CreateClient(gcpProfile *GCPProfile) (*http.Client, error) {
	dat, err := ioutil.ReadFile(gcpProfile.AuthJSONFilePath)
	if err != nil {
		return nil, errors.New("Unable to read service account file: " + err.Error())
	}

	conf, err := google.JWTConfigFromJSON(dat, defaultScopes...)
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
	return gcpCluster.KeyPair.Pem
}

func (gcpCluster *GCPCluster) KeyName() string {
	return gcpCluster.Name + "-key"
}

func CreateUniqueClusterId(deploymentName string) string {
	timeSeq := strconv.FormatInt(time.Now().Unix(), 10)
	return fmt.Sprintf("%s-%s", strings.Split(deploymentName, "-")[0], timeSeq)
}

func UploadFilesToStorage(config *viper.Viper, fileName string, filePath string) error {
	gcpProfile := &GCPProfile{
		AuthJSONFilePath: config.GetString("gcpServiceAccountJSONFile"),
	}
	client, err := CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	storageSrv, err := storage.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform storage service: " + err.Error())
	}

	file, err := os.Open(filePath)
	if err != nil {
		return errors.New("unable to open file: " + err.Error())
	}
	defer file.Close()

	projectId, err := gcpProfile.GetProjectId()
	if err != nil {
		return errors.New("Unable to find projectId: " + err.Error())
	}
	gcpProfile.ProjectId = projectId

	bucketName := fmt.Sprintf("%s-%s", projectId, config.GetString("gcpUserProfileBucketName"))
	_, err = storageSrv.Buckets.Get(bucketName).Do()
	if err != nil {
		glog.Warningf("unable to get %s bucketName from google cloud platform storage: %s", bucketName, err.Error())
		if _, err := storageSrv.Buckets.
			Insert(projectId, &storage.Bucket{Name: bucketName}).Do(); err != nil {
			return errors.New("unable to create bucketName from google cloud platform storage: " + err.Error())
		}
	}

	_, err = storageSrv.Objects.
		Insert(bucketName, &storage.Object{Name: fileName}).
		Media(file).
		Do()
	if err != nil {
		return errors.New("unable to upload file to google cloud platform storage: " + err.Error())
	}

	return nil
}

func RemoveFileFromStorage(config *viper.Viper, bucketName string, fileName string) error {
	gcpProfile := &GCPProfile{
		AuthJSONFilePath: config.GetString("gcpServiceAccountJSONFile"),
	}
	client, err := CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	storageSrv, err := storage.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform storage service: " + err.Error())
	}

	projectId, err := gcpProfile.GetProjectId()
	if err != nil {
		return errors.New("Unable to find projectId: " + err.Error())
	}
	gcpProfile.ProjectId = projectId

	_, err = storageSrv.Buckets.Get(bucketName).Do()
	if err != nil {
		return fmt.Errorf("unable to get %s bucketName from google cloud platform storage: %s", bucketName, err.Error())
	}

	err = storageSrv.Objects.Delete(bucketName, fileName).Do()
	if err != nil {
		return fmt.Errorf("unable to delete %s file from %s bucket: %s",
			fileName, bucketName, err.Error())
	}

	return nil
}

func DownloadUserProfiles(config *viper.Viper) error {
	gcpProfile := &GCPProfile{
		AuthJSONFilePath: config.GetString("gcpServiceAccountJSONFile"),
	}
	client, err := CreateClient(gcpProfile)
	if err != nil {
		return errors.New("Unable to create google cloud platform client: " + err.Error())
	}

	storageSrv, err := storage.New(client)
	if err != nil {
		return errors.New("Unable to create google cloud platform storage service: " + err.Error())
	}

	projectId, err := gcpProfile.GetProjectId()
	if err != nil {
		return errors.New("Unable to find projectId: " + err.Error())
	}
	gcpProfile.ProjectId = projectId

	bucketName := fmt.Sprintf("%s-%s", projectId, config.GetString("gcpUserProfileBucketName"))
	resp, err := storageSrv.Objects.List(bucketName).Do()
	if err != nil {
		return errors.New("unable to list bucket files from google cloud platform storage: " + err.Error())
	}

	basePath := config.GetString("filesPath")
	for _, obj := range resp.Items {
		fileName := obj.Name
		resp, err := storageSrv.Objects.
			Get(bucketName, fileName).
			Download()
		if err != nil {
			return fmt.Errorf("Unable download %s fileName: %s", fileName, err.Error())
		}
		defer resp.Body.Close()

		output, err := os.Create(basePath + "/" + fileName)
		if err != nil {
			return errors.New("Error while creating: " + err.Error())
		}
		defer output.Close()

		_, err = io.Copy(output, resp.Body)
		if err != nil {
			return errors.New("Error while downloading: " + err.Error())
		}
	}

	return nil
}
