package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/glog"
	"github.com/hyperpilotio/blobstore"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	"github.com/hyperpilotio/deployer/clusters"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/go-utils/funcs"
	"github.com/spf13/viper"

	"net/http"
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
	DEPLOYING = 6
)

// Per deployment tracking struct for the server
type DeploymentInfo struct {
	Deployer   clustermanagers.Deployer `json:"-"`
	Deployment *apis.Deployment         `json:"Deployment"`
	TemplateId string                   `json:"TemplateId"`
	Created    time.Time                `json:"Created"`
	ShutDown   time.Time                `json:"ShutDown"`
	State      DeploymentState          `json:"State"`
	Error      string                   `json:"Error"`
}

type DeploymentUserProfile struct {
	AWSProfile *hpaws.AWSProfile
	GCPProfile *hpgcp.GCPProfile
}

func (userProfile *DeploymentUserProfile) GetAWSProfile() *hpaws.AWSProfile {
	return userProfile.AWSProfile
}

func (userProfile *DeploymentUserProfile) GetGCPProfile() *hpgcp.GCPProfile {
	return userProfile.GCPProfile
}

func (info *DeploymentInfo) SetState(state DeploymentState) {
	info.State = state
	info.Error = ""
}

func (info *DeploymentInfo) SetFailure(error string) {
	info.State = FAILED
	info.Error = error
}

// This defines what's being persisted in store
type StoreDeployment struct {
	Name        string
	Region      string
	Type        string
	Status      string
	Created     string
	KeyMaterial string
	UserId      string
	Deployment  string
	TemplateId  string
	// Stores cluster manager specific stored information
	ClusterManager interface{}
}

type StoreTemplateDeployment struct {
	TemplateId string
	Deployment string
}

func (deploymentInfo *DeploymentInfo) GetDeploymentType() string {
	return deploymentInfo.Deployment.ClusterType
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

func ParseStateString(state string) DeploymentState {
	switch state {
	case "Available":
		return AVAILABLE
	case "Creating":
		return CREATING
	case "Updating":
		return UPDATING
	case "Deleting":
		return DELETING
	case "Deleted":
		return DELETED
	case "Failed":
		return FAILED
	}

	return -1
}

// Server store the stats / data of every deployment
type Server struct {
	Config                   *viper.Viper
	DeploymentStore          blobstore.BlobStore
	InClusterDeploymentStore blobstore.BlobStore
	AWSProfileStore          blobstore.BlobStore
	GCPProfileStore          blobstore.BlobStore
	TemplateStore            blobstore.BlobStore

	// Maps all available users
	DeploymentUserProfiles map[string]clusters.UserProfile

	// Maps deployment name to deployed cluster struct
	DeployedClusters map[string]*DeploymentInfo

	// Maps file id to location on disk
	UploadedFiles map[string]string

	// Maps template id to templates
	Templates map[string]*apis.Deployment

	mutex sync.Mutex
}

// NewServer return an instance of Server struct.
func NewServer(config *viper.Viper) *Server {
	return &Server{
		Config:           config,
		DeployedClusters: make(map[string]*DeploymentInfo),
		UploadedFiles:    make(map[string]string),
		Templates:        make(map[string]*apis.Deployment),
	}
}

// NewStoreDeployment create deployment that needs to be stored
func (deploymentInfo *DeploymentInfo) NewStoreDeployment() (*StoreDeployment, error) {
	b, err := json.Marshal(deploymentInfo.Deployment)
	if err != nil {
		return nil, errors.New("Unable to marshal deployment to json: " + err.Error())
	}

	storeDeployment := &StoreDeployment{
		Name:           deploymentInfo.Deployment.Name,
		UserId:         deploymentInfo.Deployment.UserId,
		Region:         deploymentInfo.Deployment.Region,
		TemplateId:     deploymentInfo.TemplateId,
		Deployment:     string(b),
		Status:         GetStateString(deploymentInfo.State),
		Created:        deploymentInfo.Created.Format(time.RFC822),
		Type:           deploymentInfo.GetDeploymentType(),
		ClusterManager: deploymentInfo.Deployer.GetStoreInfo(),
	}

	cluster := deploymentInfo.Deployer.GetCluster()
	if cluster.GetKeyMaterial() != "" {
		storeDeployment.KeyMaterial = cluster.GetKeyMaterial()
	}

	return storeDeployment, nil
}

// StartServer start a web servers
func (server *Server) StartServer() error {
	if server.Config.GetString("filesPath") == "" {
		return errors.New("filesPath is not specified in the configuration file.")
	}

	if err := os.Mkdir(server.Config.GetString("filesPath"), 0755); err != nil {
		if !os.IsExist(err) {
			return errors.New("Unable to create filesPath directory: " + err.Error())
		}
	}

	if deploymentStore, err := blobstore.NewBlobStore("Deployments", server.Config); err != nil {
		return errors.New("Unable to create deployments store: " + err.Error())
	} else {
		server.DeploymentStore = deploymentStore
	}

	if inClusterDeploymentStore, err := blobstore.NewBlobStore("InClusterDeployments", server.Config); err != nil {
		return errors.New("Unable to create in-cluster deployments store: " + err.Error())
	} else {
		server.InClusterDeploymentStore = inClusterDeploymentStore
	}

	if awsProfileStore, err := blobstore.NewBlobStore("AWSProfiles", server.Config); err != nil {
		return errors.New("Unable to create awsProfiles store: " + err.Error())
	} else {
		server.AWSProfileStore = awsProfileStore
	}

	if gcpProfileStore, err := blobstore.NewBlobStore("GCPProfiles", server.Config); err != nil {
		return errors.New("Unable to create awsProfiles store: " + err.Error())
	} else {
		server.GCPProfileStore = gcpProfileStore
	}

	if templateStore, err := blobstore.NewBlobStore("Templates", server.Config); err != nil {
		return errors.New("Unable to create templates store: " + err.Error())
	} else {
		server.TemplateStore = templateStore
	}

	if err := server.reloadClusterState(); err != nil {
		return errors.New("Unable to reload cluster state: " + err.Error())
	}

	router := gin.New()

	// Global middleware
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	router.LoadHTMLGlob(filepath.Join(os.Getenv("GOPATH"),
		"src/github.com/hyperpilotio/deployer/ui/*.html"))
	router.Static("/static", filepath.Join(os.Getenv("GOPATH"),
		"src/github.com/hyperpilotio/deployer/ui/static"))

	uiGroup := router.Group("/ui")
	{
		uiGroup.GET("", server.logUI)
		uiGroup.GET("/logs/:logFile", server.getDeploymentLogContent)
		uiGroup.GET("/list/:status", server.refreshUI)

		uiGroup.GET("/users", server.userUI)
		uiGroup.POST("/users", server.storeUser)
		uiGroup.GET("/users/:userId", server.getUser)
		uiGroup.DELETE("/users/:userId", server.deleteUser)
		uiGroup.PUT("/users/:userId", server.storeUser)
	}

	usersGroup := router.Group("/v1/users")
	{
		usersGroup.POST("/:userId/deployments", server.createDeployment)
		usersGroup.PUT("/:userId/deployments/:deployment", server.updateDeployment)

		usersGroup.POST("/:userId/files/:fileId", server.uploadFile)
	}

	daemonsGroup := router.Group("/v1/deployments")
	{
		daemonsGroup.GET("", server.getAllDeployments)
		daemonsGroup.GET("/:deployment", server.getDeployment)
		daemonsGroup.POST("", server.createDeployment)
		daemonsGroup.DELETE("/:deployment", server.deleteDeployment)
		daemonsGroup.PUT("/:deployment", server.updateDeployment)

		daemonsGroup.GET("/:deployment/ssh_key", server.getPemFile)
		daemonsGroup.GET("/:deployment/kubeconfig", server.getKubeConfigFile)
		daemonsGroup.GET("/:deployment/state", server.getDeploymentState)

		daemonsGroup.GET("/:deployment/services/:service/url", server.getServiceUrl)
		daemonsGroup.GET("/:deployment/services/:service/address", server.getServiceAddress)
		daemonsGroup.GET("/:deployment/services", server.getServices)
	}

	awsRegionGroup := router.Group("/v1/aws/regions")
	{
		awsRegionGroup.GET("/:region/availabilityZones/:availabilityZone/instances", server.getAWSRegionInstances)
	}

	templateGroup := router.Group("/v1/templates")
	{
		templateGroup.POST("/:templateId", server.storeTemplateFile)
		templateGroup.POST("/:templateId/deployments", server.createDeployment)
		templateGroup.PUT("/:templateId/deployments/:deployment/reset", server.resetTemplateDeployment)
		templateGroup.PUT("/:templateId/deployments/:deployment/deploy", server.deployExtensions)
	}

	filesGroup := router.Group("/v1/files")
	{
		filesGroup.GET("", server.getFiles)
		filesGroup.POST("/:fileId", server.uploadFile)
		filesGroup.DELETE("/:fileId", server.deleteFile)
	}

	return router.Run(":" + server.Config.GetString("port"))
}

func (server *Server) getNodeAddressForTask(c *gin.Context) {
	deploymentName := c.Param("deployment")
	taskName := c.Param("task")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment",
		})
		return
	}

	serviceAddress, err := deploymentInfo.Deployer.GetServiceAddress(taskName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find task node address:" + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  serviceAddress.Host,
	})
}

func (server *Server) storeUploadedFile(fileId string, userId string, c *gin.Context) (*string, error) {
	file, _, err := c.Request.FormFile("upload")
	tempFile := "/tmp/" + userId + "_" + fileId + ".tmp"
	destination := path.Join(server.Config.GetString("filesPath"), userId+"_"+fileId)
	out, err := os.Create(tempFile)
	defer func() {
		out.Close()
		os.Remove(tempFile)
	}()
	if err != nil {
		return nil, errors.New("Unable to create temporary file: " + err.Error())
	}
	_, err = io.Copy(out, file)
	if err != nil {
		return nil, errors.New("Unable to write to temporary file: " + err.Error())
	}

	if err := os.Rename(tempFile, destination); err != nil {
		return nil, errors.New("Unable to rename file: " + err.Error())
	}

	return &destination, nil
}

func (server *Server) getFiles(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  server.UploadedFiles,
	})
}

func (server *Server) uploadFile(c *gin.Context) {
	userId := c.Param("userId")
	if userId == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find userId",
		})
		return
	}

	fileId := c.Param("fileId")
	server.mutex.Lock()
	defer server.mutex.Unlock()

	file, err := server.storeUploadedFile(fileId, userId, c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  err.Error(),
		})
		return
	}

	server.UploadedFiles[userId+"_"+fileId] = *file

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) deleteFile(c *gin.Context) {
	// TODO implement function to delete file upload

	c.JSON(http.StatusNotImplemented, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) updateDeployment(c *gin.Context) {
	deploymentName := c.Param("deployment")

	// TODO Implement function to update deployment
	deployment := &apis.Deployment{
		UserId: c.Param("userId"),
	}

	if err := c.BindJSON(deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	if deploymentInfo.State != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Deployment is not available",
		})
		return
	}

	deployment.Name = deploymentName
	deploymentInfo.SetState(UPDATING)
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()

		if err := deploymentInfo.Deployer.UpdateDeployment(deployment); err != nil {
			log.Logger.Error("Unable to update deployment: " + err.Error())
			deploymentInfo.SetFailure(err.Error())
		} else {
			log.Logger.Infof("Update deployment successfully!")
			deploymentInfo.Deployment = deployment
			deploymentInfo.SetState(AVAILABLE)
		}
		server.storeDeployment(deploymentInfo)
	}()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "Start to update deployment " + deploymentName + "......",
	})
}

func (server *Server) getAllDeployments(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  server.DeployedClusters,
	})
}

func (server *Server) getDeployment(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if data, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		c.JSON(http.StatusOK, gin.H{
			"error": false,
			"data":  data,
		})
	} else {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
	}
}

func (server *Server) createDeployment(c *gin.Context) {
	// FIXME document the structure of deployment in the doc file
	deployment := &apis.Deployment{
		UserId: c.Param("userId"),
	}

	if err := c.BindJSON(deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	templateId := c.Param("templateId")
	if templateId != "" {
		mergeDeployment, mergeErr := server.mergeNewDeployment(templateId, deployment)
		if mergeErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Error merge template deployment: " + mergeErr.Error(),
			})
			return
		}
		deployment = mergeDeployment
	}

	deploymentInfo := &DeploymentInfo{
		TemplateId: templateId,
		Deployment: deployment,
		Created:    time.Now(),
		State:      CREATING,
	}
	deploymentType := deploymentInfo.GetDeploymentType()

	var userProfile clusters.UserProfile
	if !server.Config.GetBool("inCluster") {
		server.mutex.Lock()
		deploymentProfile, profileOk := server.DeploymentUserProfiles[deployment.UserId]
		server.mutex.Unlock()

		if !profileOk {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "User not found: " + deployment.UserId,
			})
			return
		}
		userProfile = deploymentProfile
	}

	deployer, err := clustermanagers.NewDeployer(
		server.Config,
		userProfile,
		deploymentType,
		deployment,
		true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error initialize cluster deployer: " + err.Error(),
		})
		return
	}
	deploymentInfo.Deployer = deployer

	server.mutex.Lock()
	defer server.mutex.Unlock()
	_, clusterOk := server.DeployedClusters[deployment.Name]
	if clusterOk {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}

	server.DeployedClusters[deployment.Name] = deploymentInfo

	go func() {
		log := deployer.GetLog()

		if resp, err := deployer.CreateDeployment(server.UploadedFiles); err != nil {
			log.Logger.Infof("Unable to create deployment: " + err.Error())
			deploymentInfo.SetFailure(err.Error())
		} else {
			log.Logger.Infof("Create deployment successfully!")
			deploymentInfo.Created = time.Now()

			if resp != nil {
				resJson, _ := json.Marshal(resp)
				log.Logger.Infof(string(resJson))
			}

			if err := server.NewShutDownScheduler(deployer, deploymentInfo, ""); err != nil {
				glog.Warningf("Unable to New  %s auto shutdown scheduler", deployment.Name)
			}
			deploymentInfo.SetState(AVAILABLE)

			server.mutex.Lock()
			server.DeployedClusters[deployment.Name] = deploymentInfo
			server.mutex.Unlock()
		}

		server.storeDeployment(deploymentInfo)
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error":        false,
		"data":         "Creating deployment " + deployment.Name + "......",
		"deploymentId": deployment.Name,
	})
}

func (server *Server) deleteDeployment(c *gin.Context) {
	deploymentName := c.Param("deployment")

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  deploymentName + " not found.",
		})
		return
	}

	if deploymentInfo.State != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  deploymentName + " is not available to delete",
		})
		return
	}

	scheduler := deploymentInfo.Deployer.GetScheduler()
	if scheduler != nil {
		scheduler.Stop()
	}
	deploymentInfo.SetState(DELETING)
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()
		defer log.LogFile.Close()

		if err := deploymentInfo.Deployer.DeleteDeployment(); err != nil {
			log.Logger.Errorf("Unable to delete deployment: %s", err.Error())
			deploymentInfo.SetFailure(err.Error())
		} else {
			log.Logger.Infof("Delete deployment successfully!")
			deploymentInfo.SetState(DELETED)
		}
		server.storeDeployment(deploymentInfo)

		server.mutex.Lock()
		delete(server.DeployedClusters, deploymentName)
		server.mutex.Unlock()
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "Start to delete deployment " + deploymentName + "......",
	})
}

func (server *Server) resetTemplateDeployment(c *gin.Context) {
	deploymentName := c.Param("deployment")
	templateId := c.Param("templateId")

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	if deploymentInfo.State != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Deployment is not available",
		})
		return
	}

	templateDeployment, ok := server.Templates[templateId]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Template not found",
		})
		return
	}

	deployment := &apis.Deployment{}
	funcs.DeepCopy(deployment, templateDeployment)

	deployment.UserId = deploymentInfo.Deployment.UserId
	deployment.Name = deploymentName
	deploymentInfo.SetState(UPDATING)
	server.mutex.Unlock()

	log := deploymentInfo.Deployer.GetLog()

	go func() {
		log.Logger.Infof("Resetting deployment to template %s: %+v", templateId, deployment)

		if err := deploymentInfo.Deployer.UpdateDeployment(deployment); err != nil {
			log.Logger.Error("Unable to reset template deployment: " + err.Error())
			deploymentInfo.SetFailure(err.Error())
		} else {
			log.Logger.Infof("Reset template deployment successfully!")
			deploymentInfo.Deployment = deployment
			deploymentInfo.SetState(AVAILABLE)
		}

		if err := server.DeploymentStore.Delete(deploymentName); err != nil {
			log.Logger.Errorf("Unable to delete %s deployment status: %s", deploymentName, err.Error())
		}
		server.storeDeployment(deploymentInfo)
	}()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "Starting to reset template deployment " + deploymentName + "......",
	})
}

func (server *Server) deployExtensions(c *gin.Context) {
	deploymentName := c.Param("deployment")
	templateId := c.Param("templateId")

	deployment := &apis.Deployment{}
	if err := c.BindJSON(deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	if templateId == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Empty template passed",
		})
		return
	}

	newDeployment, err := server.mergeNewDeployment(templateId, deployment)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to merge new deployment: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	// We allow failed deployment to retry for extensions
	if deploymentInfo.State != AVAILABLE && deploymentInfo.State != FAILED {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Deployment is not available, state: " + GetStateString(deploymentInfo.State),
		})
		return
	}

	deploymentInfo.SetState(UPDATING)
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()

		log.Logger.Infof("Deplyoing extensions: %+v", deployment)
		log.Logger.Infof("New merged deployment manifest: %+v", newDeployment)

		if err := deploymentInfo.Deployer.DeployExtensions(deployment, newDeployment); err != nil {
			log.Logger.Error("Unable to deploy extensions deployment: " + err.Error())
			deploymentInfo.SetFailure(err.Error())
		} else {
			log.Logger.Infof("Deploy extensions deployment successfully!")
			deploymentInfo.Deployment = newDeployment
			deploymentInfo.SetState(AVAILABLE)
		}

		if err := server.storeDeployment(deploymentInfo); err != nil {
			log.Logger.Error("Unable to store deployment: " + err.Error())
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "Start to deploy extensions deployment " + deploymentName + "......",
	})
}

func (server *Server) getPemFile(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if deploymentInfo, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		keyMaterial := deploymentInfo.Deployer.GetCluster().GetKeyMaterial()
		if keyMaterial != "" {
			privateKey := strings.Replace(keyMaterial, "\\n", "\n", -1)
			c.String(http.StatusOK, privateKey)
			return
		}
	}

	c.JSON(http.StatusNotFound, gin.H{
		"error": true,
		"data":  c.Param("deployment") + " not found.",
	})
}

func (server *Server) getKubeConfigFile(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if deploymentInfo, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		kubeConfigPath, err := deploymentInfo.Deployer.GetKubeConfigPath()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to get kubeConfig file:" + err.Error(),
			})
			return
		}

		if kubeConfigPath != "" {
			if b, err := ioutil.ReadFile(kubeConfigPath); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": true,
					"data":  "Unable to read kubeConfig file: " + err.Error(),
				})
			} else {
				c.String(http.StatusOK, string(b))
			}
			return
		}
	}

	c.JSON(http.StatusNotFound, gin.H{
		"error": true,
		"data":  c.Param("deployment") + " not found.",
	})
}

func (server *Server) getDeploymentState(c *gin.Context) {
	deploymentName := c.Param("deployment")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  deploymentName + " not found.",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"state": GetStateString(deploymentInfo.State),
		"data":  deploymentInfo.Error,
	})
}

func (server *Server) getServiceUrl(c *gin.Context) {
	deploymentName := c.Param("deployment")
	serviceName := c.Param("service")

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	server.mutex.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment",
		})
		return
	}

	serviceUrl, err := deploymentInfo.Deployer.GetServiceUrl(serviceName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to get service url: " + err.Error(),
		})
		return
	}

	c.String(http.StatusOK, serviceUrl)
}

func (server *Server) getServiceAddress(c *gin.Context) {
	deploymentName := c.Param("deployment")
	serviceName := c.Param("service")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment",
		})
		return
	}

	serviceAddress, err := deploymentInfo.Deployer.GetServiceAddress(serviceName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to get service url: " + err.Error(),
		})
		return
	}

	address, err := json.Marshal(serviceAddress)
	if err != nil {
		return
	}

	c.String(http.StatusOK, string(address))
}

func (server *Server) getServices(c *gin.Context) {
	deploymentName := c.Param("deployment")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentInfo, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment: " + deploymentName,
		})
		return
	}

	services, err := deploymentInfo.Deployer.GetServiceMappings()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to get service mappings: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  services,
	})
}

func (server *Server) getAWSRegionInstances(c *gin.Context) {
	regionName := c.Param("region")
	availabilityZoneName := c.Param("availabilityZone")

	if regionName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Empty region passed",
		})
		return
	}

	if availabilityZoneName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Empty availability zone passed",
		})
		return
	}

	instances, err := awsecs.GetSupportInstanceTypes(&hpaws.AWSProfile{
		AwsId:     server.Config.GetString("awsId"),
		AwsSecret: server.Config.GetString("awsSecret"),
	}, regionName, availabilityZoneName)

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to get supported aws instances: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error":     false,
		"instances": instances,
	})
}

func (server *Server) storeDeployment(deploymentInfo *DeploymentInfo) error {
	deploymentName := deploymentInfo.Deployment.Name
	var deploymentStore blobstore.BlobStore
	if server.Config.GetBool("inCluster") {
		deploymentStore = server.InClusterDeploymentStore
	} else {
		deploymentStore = server.DeploymentStore
	}

	switch deploymentInfo.State {
	case DELETED:
		glog.Infof("Deleting deployment from store: " + deploymentName)
		if err := deploymentStore.Delete(deploymentName); err != nil {
			return fmt.Errorf("Unable to delete %s deployment status: %s", deploymentName, err.Error())
		}
	default:
		glog.Infof("Storing deployment: " + deploymentName)
		deployment, err := deploymentInfo.NewStoreDeployment()
		if err != nil {
			return fmt.Errorf("Unable to new %s store deployment: %s", deploymentName, err.Error())
		}

		if err := deploymentStore.Store(deploymentName, deployment); err != nil {
			return fmt.Errorf("Unable to store %s deployment status: %s", deploymentName, err.Error())
		}
	}

	return nil
}

func (server *Server) storeTemplateFile(c *gin.Context) {
	deployment := &apis.Deployment{
		UserId: "",
	}
	if err := c.BindJSON(&deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	b, jsonErr := json.Marshal(deployment)
	if jsonErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to marshal deployment to json: " + jsonErr.Error(),
		})
		return
	}

	templateId := c.Param("templateId")
	templateDeployment := &StoreTemplateDeployment{
		TemplateId: templateId,
		Deployment: string(b),
	}

	if err := server.TemplateStore.Store(templateId, templateDeployment); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  fmt.Errorf("Unable to store %s templates: %s", templateId, err.Error()),
		})
		return
	}

	server.mutex.Lock()
	server.Templates[templateId] = deployment
	server.mutex.Unlock()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) mergeNewDeployment(templateId string, newDeployment *apis.Deployment) (*apis.Deployment, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	templateDeployment, ok := server.Templates[templateId]
	if !ok {
		return nil, fmt.Errorf("Unable to find %s deployment templates", templateId)
	}

	newTemplateDeployment := &apis.Deployment{}
	funcs.DeepCopy(templateDeployment, newTemplateDeployment)

	if newDeployment.UserId != "" {
		newTemplateDeployment.UserId = newDeployment.UserId
	}

	if newDeployment.Name != "" {
		newTemplateDeployment.Name = newDeployment.Name
	}

	// Allow new deployment to overwrite node id with new instance type
	newClusterNodes := []apis.ClusterNode{}
	clusterMapping := make(map[int]apis.ClusterNode)
	for _, node := range newTemplateDeployment.ClusterDefinition.Nodes {
		clusterMapping[node.Id] = node
	}
	for _, node := range newDeployment.ClusterDefinition.Nodes {
		clusterMapping[node.Id] = node
	}
	for _, node := range clusterMapping {
		newClusterNodes = append(newClusterNodes, node)
	}
	newTemplateDeployment.ClusterDefinition.Nodes = newClusterNodes

	for _, nodeMapping := range newDeployment.NodeMapping {
		if err := checkDuplicateTask(newTemplateDeployment.NodeMapping, nodeMapping); err != nil {
			return nil, fmt.Errorf("Unable to merge deployment: %s", err.Error())
		}
		newTemplateDeployment.NodeMapping =
			append(newTemplateDeployment.NodeMapping, nodeMapping)
	}

	for _, task := range newDeployment.KubernetesDeployment.Kubernetes {
		newTemplateDeployment.KubernetesDeployment.Kubernetes =
			append(newTemplateDeployment.KubernetesDeployment.Kubernetes, task)
	}

	return newTemplateDeployment, nil
}

func checkDuplicateTask(nodeMappings []apis.NodeMapping, checkNodeMapping apis.NodeMapping) error {
	for _, nodeMapping := range nodeMappings {
		if nodeMapping.Id == checkNodeMapping.Id && nodeMapping.Task == checkNodeMapping.Task {
			return fmt.Errorf("Find duplicate %s deployment task on node id %d",
				checkNodeMapping.Task, checkNodeMapping.Id)
		}
	}

	return nil
}

// reloadClusterState reload cluster state when deployer restart
func (server *Server) reloadClusterState() error {
	deploymentUserProfiles := map[string]clusters.UserProfile{}
	server.DeploymentUserProfiles = deploymentUserProfiles

	profiles, err := server.AWSProfileStore.LoadAll(func() interface{} {
		return &hpaws.AWSProfile{}
	})
	if err != nil {
		return fmt.Errorf("Unable to load aws profiles: %s", err.Error())
	}
	for _, awsProfile := range profiles.([]interface{}) {
		userId := awsProfile.(*hpaws.AWSProfile).UserId
		userProfile, ok := server.DeploymentUserProfiles[userId]
		if ok {
			server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				AWSProfile: awsProfile.(*hpaws.AWSProfile),
				GCPProfile: userProfile.GetGCPProfile(),
			}
		} else {
			server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				AWSProfile: awsProfile.(*hpaws.AWSProfile),
			}
		}
	}

	if err := hpgcp.DownloadUserProfiles(server.Config); err != nil {
		return fmt.Errorf("Unable to download all GCP user profiles: %s", err.Error())
	}

	gcpProfiles, err := server.GCPProfileStore.LoadAll(func() interface{} {
		return &hpgcp.GCPProfile{}
	})
	if err != nil {
		return fmt.Errorf("Unable to load gcp profiles: %s", err.Error())
	}
	for _, gcpProfile := range gcpProfiles.([]interface{}) {
		userId := gcpProfile.(*hpgcp.GCPProfile).UserId
		userProfile, ok := server.DeploymentUserProfiles[userId]
		if ok {
			server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				AWSProfile: userProfile.GetAWSProfile(),
				GCPProfile: gcpProfile.(*hpgcp.GCPProfile),
			}
		} else {
			server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				GCPProfile: gcpProfile.(*hpgcp.GCPProfile),
			}
		}
	}

	inCluster := server.Config.GetBool("inCluster")
	var deploymentStore blobstore.BlobStore
	if inCluster {
		deploymentStore = server.InClusterDeploymentStore
	} else {
		deploymentStore = server.DeploymentStore
	}

	deployments, err := deploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{}
	})
	if err != nil {
		return fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	templates, templateErr := server.TemplateStore.LoadAll(func() interface{} {
		return &StoreTemplateDeployment{}
	})
	if templateErr != nil {
		return fmt.Errorf("Unable to load deployment templates: %s", templateErr.Error())
	}

	glog.V(1).Infof("Loading %d template deployment from store", len(templates.([]interface{})))
	for _, template := range templates.([]interface{}) {
		templateDeployment := template.(*StoreTemplateDeployment)
		deploymentJSON := template.(*StoreTemplateDeployment).Deployment

		deployment := &apis.Deployment{}
		if err := json.Unmarshal([]byte(deploymentJSON), deployment); err != nil {
			glog.Warningf("Skip loading template deployment %s: Unmarshal error", templateDeployment.TemplateId)
			continue
		}

		glog.V(1).Infof("Recovered template %s deployment from store", templateDeployment.TemplateId)
		server.Templates[templateDeployment.TemplateId] = deployment
	}

	shutdownTime := server.Config.GetString("shutDownTime")
	if shutdownTime == "" {
		shutdownTime = "12h"
	}
	scheduleRunTime, err := time.ParseDuration(shutdownTime)
	if err != nil {
		return fmt.Errorf("Unable to parse shutDownTime %s: %s", scheduleRunTime, err.Error())
	}

	for _, deployment := range deployments.([]interface{}) {
		storeDeployment := deployment.(*StoreDeployment)
		deploymentName := storeDeployment.Name
		glog.Infof("Trying to recover deployment %s from store", deploymentName)
		glog.V(2).Infof("Deployment found in store: %+v", deployment)
		if storeDeployment.Status == "Deleted" || storeDeployment.Status == "Failed" {
			// TODO: Remove failed stored deployments
			continue
		}

		var userProfile clusters.UserProfile
		if !inCluster {
			userId := storeDeployment.UserId
			if userId == "" {
				glog.Warningf("Skip loading deployment %s: Empty user id", storeDeployment.Name)
				continue
			}

			deploymentProfile, profileOk := server.DeploymentUserProfiles[storeDeployment.UserId]
			if !profileOk {
				glog.Warning("Skip loading deployment: Unable to find aws profile for user " + storeDeployment.UserId)
				continue
			}
			userProfile = deploymentProfile
		}

		deployment := &apis.Deployment{}
		unmarshalErr := json.Unmarshal([]byte(storeDeployment.Deployment), deployment)
		if unmarshalErr != nil {
			glog.Warning("Skip loading deployment: Unable to load deployment manifest for deployment " + deploymentName)
			continue
		}

		deployer, err := clustermanagers.NewDeployer(
			server.Config,
			userProfile,
			storeDeployment.Type,
			deployment,
			false)
		if err != nil {
			return fmt.Errorf("Error initialize %s deployer %s", deploymentName, err.Error())
		}

		storeClusterManager := deployer.NewStoreInfo()
		if storeClusterManager != nil {
			storeDeployment.ClusterManager = storeClusterManager
			if err := deploymentStore.Load(storeDeployment.Name, storeDeployment); err != nil {
				glog.Warningf("Skipping reloading deployment %s: Unable to load cluster store info: %s", deploymentName, err.Error())
				continue
			}
		}

		deploymentInfo := &DeploymentInfo{
			Deployer:   deployer,
			Deployment: deployment,
			TemplateId: storeDeployment.TemplateId,
			Created:    time.Now(),
			State:      ParseStateString(storeDeployment.Status),
		}

		// Reload keypair
		if !inCluster {
			glog.Infof("Reloading key pair for deployment %s", deployment.Name)
			cluster := deploymentInfo.Deployer.GetCluster()
			if err := cluster.ReloadKeyPair(storeDeployment.KeyMaterial); err != nil {
				if err := deploymentStore.Delete(deploymentName); err != nil {
					glog.Warningf("Unable to delete %s deployment after reload keyPair: %s", deploymentName, err.Error())
				}
				glog.Warningf("Skipping reloading because unable to load %s keyPair: %s", deploymentName, err.Error())
				continue
			}
		}

		glog.Infof("Reloading cluster state for deployment: %s", deployment.Name)
		if err := deployer.ReloadClusterState(storeClusterManager); err != nil {
			if err := deploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after failed reload: %s", deploymentName, err.Error())
			}
			glog.Warningf("Unable to reload cluster state for deployment %s: %s", deploymentName, err.Error())
			continue
		}

		deploymentInfo.State = ParseStateString(storeDeployment.Status)
		newScheduleRunTime := ""
		if createdTime, err := time.Parse(time.RFC822, storeDeployment.Created); err == nil {
			deploymentInfo.Created = createdTime
			realScheduleRunTime := createdTime.Add(scheduleRunTime)
			if realScheduleRunTime.After(time.Now()) {
				newScheduleRunTime = realScheduleRunTime.Sub(time.Now()).String()
			}
		}

		server.DeployedClusters[deploymentName] = deploymentInfo

		if deploymentInfo.State == AVAILABLE {
			if err := server.NewShutDownScheduler(deployer, deploymentInfo, newScheduleRunTime); err != nil {
				glog.Warningf("Unable to create auto shutdown scheduler for %s: %s", deployment.Name, err.Error())
			}
		}
	}

	return nil
}

func (server *Server) NewShutDownScheduler(
	deployer clustermanagers.Deployer,
	deploymentInfo *DeploymentInfo,
	custScheduleRunTime string) error {
	if deployer.GetScheduler() != nil {
		deployer.GetScheduler().Stop()
	}

	scheduleRunTime := custScheduleRunTime
	if scheduleRunTime == "" {
		if deploymentInfo.Deployment.ShutDownTime != "" {
			scheduleRunTime = deploymentInfo.Deployment.ShutDownTime
		} else {
			scheduleRunTime = server.Config.GetString("shutDownTime")
		}
	}

	startTime, err := time.ParseDuration(scheduleRunTime)
	if err != nil {
		return fmt.Errorf("Unable to parse shutDownTime %s: %s", scheduleRunTime, err.Error())
	}

	shutDownTime := time.Now().Add(startTime)
	deploymentInfo.ShutDown = shutDownTime
	glog.Infof("New %s schedule at %s", deploymentInfo.Deployment.Name, shutDownTime)

	scheduler := job.NewScheduler(startTime, func() {
		go func() {
			server.mutex.Lock()
			if deploymentInfo.State == DELETING {
				server.mutex.Unlock()
				glog.Infof("Skip deleting deployment %s on schedule as it's currently being deleted",
					deploymentInfo.Deployment.Name)
				return
			}

			deploymentInfo.SetState(DELETING)
			server.mutex.Unlock()

			defer deployer.GetLog().LogFile.Close()

			if err := deployer.DeleteDeployment(); err != nil {
				deployer.GetLog().Logger.Infof("Deployment %s failed to delete on schedule: %s",
					deploymentInfo.Deployment.Name, err.Error())
				deploymentInfo.SetFailure(err.Error())
			} else {
				deploymentInfo.SetState(DELETED)
			}
			server.storeDeployment(deploymentInfo)

			server.mutex.Lock()
			delete(server.DeployedClusters, deploymentInfo.Deployment.Name)
			server.mutex.Unlock()
		}()
	})

	deployer.SetScheduler(scheduler)

	return nil
}
