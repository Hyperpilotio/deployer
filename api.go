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

	"github.com/aws/aws-sdk-go/aws"
	"github.com/gin-gonic/gin"
	"github.com/golang/glog"
	"github.com/hyperpilotio/blobstore"
	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	"github.com/hyperpilotio/deployer/job"
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
	if deploymentInfo.Deployment.KubernetesDeployment != nil {
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
	ProfileStore             blobstore.BlobStore
	TemplateStore            blobstore.BlobStore
	OriginalDeployer         clustermanagers.Deployer

	// Maps all available users
	AWSProfiles map[string]*hpaws.AWSProfile

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

	awsCluster := deploymentInfo.Deployer.GetAWSCluster()
	if awsCluster.KeyPair != nil {
		storeDeployment.KeyMaterial = aws.StringValue(awsCluster.KeyPair.KeyMaterial)
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

	if profileStore, err := blobstore.NewBlobStore("AWSProfiles", server.Config); err != nil {
		return errors.New("Unable to create awsProfiles store: " + err.Error())
	} else {
		server.ProfileStore = profileStore
	}

	if templateStore, err := blobstore.NewBlobStore("Templates", server.Config); err != nil {
		return errors.New("Unable to create templates store: " + err.Error())
	} else {
		server.TemplateStore = templateStore
	}

	if server.Config.GetBool("inCluster") {
		if err := server.reloadInClusterState(); err != nil {
			return errors.New("Unable to reload in-cluster state: " + err.Error())
		}
	} else {
		if err := server.reloadClusterState(); err != nil {
			return errors.New("Unable to reload cluster state: " + err.Error())
		}
	}

	//gin.SetMode("release")
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

		uiGroup.GET("/clusters", server.clusterUI)
		uiGroup.GET("/clusters/:deploymentType/:clusterName", server.getCluster)
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

	nodeId := -1
	for _, nodeMapping := range deploymentInfo.Deployment.NodeMapping {
		if nodeMapping.Task == taskName {
			nodeId = nodeMapping.Id
			break
		}
	}

	if nodeId == -1 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find task in deployment node mappings",
		})
		return
	}

	nodeInfo, nodeOk := deploymentInfo.Deployer.GetAWSCluster().NodeInfos[nodeId]
	if !nodeOk {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find node in cluster",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  nodeInfo.PublicDnsName,
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
	deploymentInfo.State = UPDATING
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()

		if err := deploymentInfo.Deployer.UpdateDeployment(deployment); err != nil {
			log.Logger.Error("Unable to update deployment")
			deploymentInfo.State = FAILED
		} else {
			log.Logger.Infof("Update deployment successfully!")
			deploymentInfo.Deployment = deployment
			deploymentInfo.State = AVAILABLE
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

	server.mutex.Lock()
	awsProfile, profileOk := server.AWSProfiles[deployment.UserId]
	server.mutex.Unlock()

	if !profileOk {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "User not found: " + deployment.UserId,
		})
		return
	}

	deploymentInfo := &DeploymentInfo{
		TemplateId: templateId,
		Deployment: deployment,
		Created:    time.Now(),
		State:      CREATING,
	}

	deployer, err := clustermanagers.NewDeployer(
		server.Config, awsProfile, deploymentInfo.GetDeploymentType(),
		deployment, true, server.OriginalDeployer)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error initialize cluster deployer: " + err.Error(),
		})
		return
	}
	deploymentInfo.Deployer = deployer

	server.mutex.Lock()
	_, clusterOk := server.DeployedClusters[deployment.Name]
	server.mutex.Unlock()
	if clusterOk {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}

	server.mutex.Lock()
	server.DeployedClusters[deployment.Name] = deploymentInfo
	server.mutex.Unlock()

	go func() {
		log := deployer.GetLog()

		if resp, err := deployer.CreateDeployment(server.UploadedFiles); err != nil {
			log.Logger.Infof("Unable to create deployment: " + err.Error())
			deploymentInfo.State = FAILED
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
			deploymentInfo.State = AVAILABLE

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
	deploymentInfo.State = DELETING
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()
		defer log.LogFile.Close()

		if err := deploymentInfo.Deployer.DeleteDeployment(); err != nil {
			log.Logger.Errorf("Unable to delete deployment: %s", err.Error())
			deploymentInfo.State = FAILED
		} else {
			log.Logger.Infof("Delete deployment successfully!")
			deploymentInfo.State = DELETED
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

	deployment := *templateDeployment
	deployment.UserId = deploymentInfo.Deployment.UserId
	deployment.Name = deploymentName
	deploymentInfo.State = UPDATING
	server.mutex.Unlock()

	log := deploymentInfo.Deployer.GetLog()

	go func() {
		log.Logger.Infof("Resetting deployment to template %s: %+v", templateId, deployment)

		if err := deploymentInfo.Deployer.UpdateDeployment(&deployment); err != nil {
			log.Logger.Error("Unable to reset template deployment")
			deploymentInfo.State = FAILED
		} else {
			log.Logger.Infof("Reset template deployment successfully!")
			deploymentInfo.Deployment = &deployment
			deploymentInfo.State = AVAILABLE
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

	deploymentInfo.State = UPDATING
	server.mutex.Unlock()

	go func() {
		log := deploymentInfo.Deployer.GetLog()

		log.Logger.Infof("Deplyoing extensions: %+v", deployment)
		log.Logger.Infof("New merged deployment manifest: %+v", newDeployment)

		if err := deploymentInfo.Deployer.DeployExtensions(deployment, newDeployment); err != nil {
			log.Logger.Error("Unable to deploy extensions deployment: " + err.Error())
			deploymentInfo.State = FAILED
		} else {
			log.Logger.Infof("Deploy extensions deployment successfully!")
			deploymentInfo.Deployment = newDeployment
			deploymentInfo.State = AVAILABLE
		}

		server.storeDeployment(deploymentInfo)
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
		awsCluster := deploymentInfo.Deployer.GetAWSCluster()
		if awsCluster != nil && awsCluster.KeyPair != nil {
			privateKey := strings.Replace(*awsCluster.KeyPair.KeyMaterial, "\\n", "\n", -1)
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
		if deploymentInfo.GetDeploymentType() != "K8S" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unsupported deployment type",
			})
			return
		}

		kubeConfigPath := deploymentInfo.Deployer.(*kubernetes.K8SDeployer).GetKubeConfigPath()
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

	c.String(http.StatusOK, GetStateString(deploymentInfo.State))
}

func (server *Server) getServiceUrl(c *gin.Context) {
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

	copyDeployment := *templateDeployment
	if newDeployment.UserId != "" {
		copyDeployment.UserId = newDeployment.UserId
	}

	if newDeployment.Name != "" {
		copyDeployment.Name = newDeployment.Name
	}

	// Allow new deployment to overwrite node id with new instance type
	newClusterNodes := []apis.ClusterNode{}
	clusterMapping := make(map[int]apis.ClusterNode)
	for _, node := range templateDeployment.ClusterDefinition.Nodes {
		clusterMapping[node.Id] = node
	}

	for _, node := range newDeployment.ClusterDefinition.Nodes {
		clusterMapping[node.Id] = node
	}

	for _, node := range clusterMapping {
		newClusterNodes = append(newClusterNodes, node)
	}

	copyDeployment.ClusterDefinition.Nodes = newClusterNodes

	existingMapping := copyDeployment.NodeMapping
	copyDeployment.NodeMapping = make([]apis.NodeMapping, 0)

	for _, nodeMapping := range existingMapping {
		copyDeployment.NodeMapping = append(copyDeployment.NodeMapping, nodeMapping)
	}

	for _, nodeMapping := range newDeployment.NodeMapping {
		copyDeployment.NodeMapping = append(copyDeployment.NodeMapping, nodeMapping)
	}

	copyKubernetesDeployment := *copyDeployment.KubernetesDeployment
	copyDeployment.KubernetesDeployment = &copyKubernetesDeployment

	existingKubernetesDeployment := copyDeployment.KubernetesDeployment.Kubernetes
	copyDeployment.KubernetesDeployment.Kubernetes = make([]apis.KubernetesTask, 0)

	for _, task := range existingKubernetesDeployment {
		copyDeployment.KubernetesDeployment.Kubernetes = append(copyDeployment.KubernetesDeployment.Kubernetes, task)
	}

	for _, task := range newDeployment.KubernetesDeployment.Kubernetes {
		copyDeployment.KubernetesDeployment.Kubernetes = append(copyDeployment.KubernetesDeployment.Kubernetes, task)
	}

	return &copyDeployment, nil
}

// reloadClusterState reload cluster state when deployer restart
func (server *Server) reloadClusterState() error {
	profiles, profileErr := server.ProfileStore.LoadAll(func() interface{} {
		return &hpaws.AWSProfile{}
	})
	if profileErr != nil {
		return fmt.Errorf("Unable to load aws profiles: %s", profileErr.Error())
	}

	awsProfileInfos := map[string]*hpaws.AWSProfile{}
	for _, awsProfile := range profiles.([]interface{}) {
		awsProfileInfos[awsProfile.(*hpaws.AWSProfile).UserId] = awsProfile.(*hpaws.AWSProfile)
	}
	server.AWSProfiles = awsProfileInfos

	deployments, err := server.DeploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{
			ClusterManager: &kubernetes.StoreInfo{},
		}
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

	for _, template := range templates.([]interface{}) {
		templateId := template.(*StoreTemplateDeployment)
		deploymentJSON := template.(*StoreTemplateDeployment).Deployment

		deployment := &apis.Deployment{}
		if err := json.Unmarshal([]byte(deploymentJSON), deployment); err != nil {
			glog.Warningf("Skip loading template deployment %s: Unmarshal error", templateId)
			continue
		}
		server.Templates[template.(*StoreTemplateDeployment).TemplateId] = deployment
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
		glog.V(1).Infof("Trying to recover deployment from store: %+v", storeDeployment)
		if storeDeployment.Status == "Deleted" || storeDeployment.Status == "Failed" {
			// TODO: Remove failed stored deployments
			continue
		}

		userId := storeDeployment.UserId
		if userId == "" {
			glog.Warningf("Skip loading deployment %s: Empty user id", storeDeployment.Name)
			continue
		}

		awsProfile, profileOk := server.AWSProfiles[storeDeployment.UserId]
		if !profileOk {
			glog.Warning("Skip loading deployment: Unable to find aws profile for user " + storeDeployment.UserId)
			continue
		}

		deploymentName := storeDeployment.Name

		deployment := &apis.Deployment{}
		unmarshalErr := json.Unmarshal([]byte(storeDeployment.Deployment), deployment)
		if unmarshalErr != nil {
			glog.Warning("Skip loading deployment: Unable to load deployment manifest for deployment " + deploymentName)
			continue
		}

		deployer, err := clustermanagers.NewDeployer(server.Config, awsProfile,
			storeDeployment.Type, deployment, false, server.OriginalDeployer)
		if err != nil {
			return fmt.Errorf("Error initialize %s deployer %s", deploymentName, err.Error())
		}

		deploymentInfo := &DeploymentInfo{
			Deployer:   deployer,
			Deployment: deployment,
			TemplateId: storeDeployment.TemplateId,
			Created:    time.Now(),
			State:      ParseStateString(storeDeployment.Status),
		}

		// Reload keypair
		if err := deployer.GetAWSCluster().ReloadKeyPair(storeDeployment.KeyMaterial); err != nil {
			if err := server.DeploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after reload keyPair: %s", deploymentName, err.Error())
			}
			glog.Warningf("Skipping reloading because unable to load %s keyPair: %s", deploymentName, err.Error())
			continue
		}

		if err := deployer.ReloadClusterState(storeDeployment.ClusterManager); err != nil {
			if err := server.DeploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after failed check: %s", deploymentName, err.Error())
			}
			glog.Warningf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
		} else {
			deploymentInfo.State = ParseStateString(storeDeployment.Status)
			newScheduleRunTime := ""
			createdTime, err := time.Parse(time.RFC822, storeDeployment.Created)
			if err == nil {
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
	}

	return nil
}

func (server *Server) reloadInClusterState() error {
	profiles, profileErr := server.ProfileStore.LoadAll(func() interface{} {
		return &hpaws.AWSProfile{}
	})
	if profileErr != nil {
		return fmt.Errorf("Unable to load aws profiles: %s", profileErr.Error())
	}

	inClusterUserId := ""
	inClusterAwsProfile := &hpaws.AWSProfile{}
	awsProfileInfos := map[string]*hpaws.AWSProfile{}
	for _, awsProfile := range profiles.([]interface{}) {
		if awsProfile.(*hpaws.AWSProfile).AwsId == server.Config.GetString("awsId") {
			inClusterUserId = awsProfile.(*hpaws.AWSProfile).UserId
			inClusterAwsProfile = awsProfile.(*hpaws.AWSProfile)
			awsProfileInfos[inClusterUserId] = inClusterAwsProfile
			break
		}
	}
	server.AWSProfiles = awsProfileInfos

	templates, templateErr := server.TemplateStore.LoadAll(func() interface{} {
		return &StoreTemplateDeployment{}
	})
	if templateErr != nil {
		return fmt.Errorf("Unable to load deployment templates: %s", templateErr.Error())
	}

	for _, template := range templates.([]interface{}) {
		templateId := template.(*StoreTemplateDeployment)
		deploymentJSON := template.(*StoreTemplateDeployment).Deployment

		deployment := &apis.Deployment{}
		if err := json.Unmarshal([]byte(deploymentJSON), deployment); err != nil {
			glog.Warningf("Skip loading template deployment %s: Unmarshal error", templateId)
			continue
		}
		server.Templates[template.(*StoreTemplateDeployment).TemplateId] = deployment
	}

	deployments, err := server.DeploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{
			ClusterManager: &kubernetes.StoreInfo{},
		}
	})
	if err != nil {
		return fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	for _, deployment := range deployments.([]interface{}) {
		storeDeployment := deployment.(*StoreDeployment)
		glog.V(1).Infof("Trying to recover deployment from store: %+v", storeDeployment)
		if storeDeployment.Status == "Deleted" || storeDeployment.Status == "Failed" {
			continue
		}

		if storeDeployment.UserId != inClusterUserId {
			glog.Warningf("Skip loading deployment %s: Not in-cluster user id", storeDeployment.Name)
			continue
		}

		deploymentName := storeDeployment.Name

		deployment := &apis.Deployment{}
		unmarshalErr := json.Unmarshal([]byte(storeDeployment.Deployment), deployment)
		if unmarshalErr != nil {
			glog.Warning("Skip loading deployment: Unable to load deployment manifest for deployment " + deploymentName)
			continue
		}

		inClusterDeployment := false
		for _, nodeMapping := range deployment.NodeMapping {
			if nodeMapping.Task == "deployer" {
				inClusterDeployment = true
				break
			}
		}

		if !inClusterDeployment {
			continue
		}

		server.Config.Set("inCluster", false)
		deployer, err := clustermanagers.NewDeployer(server.Config, inClusterAwsProfile,
			storeDeployment.Type, deployment, false, nil)
		if err != nil {
			return fmt.Errorf("Error initialize %s deployer %s", deploymentName, err.Error())
		}
		server.Config.Set("inCluster", true)

		// Reload keypair
		if err := deployer.GetAWSCluster().ReloadKeyPair(storeDeployment.KeyMaterial); err != nil {
			if err := server.DeploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after reload keyPair: %s", deploymentName, err.Error())
			}
			glog.Warningf("Skipping reloading because unable to load %s keyPair: %s", deploymentName, err.Error())
			continue
		}

		if err := deployer.ReloadClusterState(storeDeployment.ClusterManager); err != nil {
			if err := server.DeploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after failed check: %s", deploymentName, err.Error())
			}
			glog.Warningf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
		}

		server.OriginalDeployer = deployer
		break
	}

	inDeployments, err := server.InClusterDeploymentStore.LoadAll(func() interface{} {
		return &StoreDeployment{
			ClusterManager: &kubernetes.StoreInfo{},
		}
	})
	if err != nil {
		return fmt.Errorf("Unable to load in-cluster deployment: %s", err.Error())
	}

	for _, deployment := range inDeployments.([]interface{}) {
		storeDeployment := deployment.(*StoreDeployment)
		glog.V(1).Infof("Trying to recover deployment from store: %+v", storeDeployment)
		if storeDeployment.Status == "Deleted" || storeDeployment.Status == "Failed" {
			continue
		}

		if storeDeployment.UserId != inClusterUserId {
			glog.Warningf("Skip loading deployment %s: Not in-cluster user id", storeDeployment.Name)
			continue
		}

		deploymentName := storeDeployment.Name

		deployment := &apis.Deployment{}
		unmarshalErr := json.Unmarshal([]byte(storeDeployment.Deployment), deployment)
		if unmarshalErr != nil {
			glog.Warning("Skip loading deployment: Unable to load deployment manifest for deployment " + deploymentName)
			continue
		}

		deployer, err := clustermanagers.NewDeployer(server.Config, inClusterAwsProfile,
			storeDeployment.Type, deployment, false, server.OriginalDeployer)
		if err != nil {
			return fmt.Errorf("Error initialize %s deployer %s", deploymentName, err.Error())
		}

		deploymentInfo := &DeploymentInfo{
			Deployer:   deployer,
			Deployment: deployment,
			TemplateId: storeDeployment.TemplateId,
			Created:    time.Now(),
			State:      ParseStateString(storeDeployment.Status),
		}

		if err := deployer.ReloadClusterState(storeDeployment.ClusterManager); err != nil {
			if err := server.InClusterDeploymentStore.Delete(deploymentName); err != nil {
				glog.Warningf("Unable to delete %s deployment after failed check: %s", deploymentName, err.Error())
			}
			glog.Warningf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
		} else {
			server.DeployedClusters[deploymentName] = deploymentInfo
		}
	}

	return nil
}

func (server *Server) NewShutDownScheduler(
	deployer clustermanagers.Deployer,
	deploymentInfo *DeploymentInfo,
	custScheduleRunTime string) error {
	if server.Config.GetBool("inCluster") {
		return nil
	}
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

			deploymentInfo.State = DELETING
			server.mutex.Unlock()

			defer deployer.GetLog().LogFile.Close()

			if err := deployer.DeleteDeployment(); err != nil {
				deploymentInfo.State = FAILED
				glog.Infof("Deployment %s failed to delete: %s", deploymentInfo.Deployment.Name, err.Error())
			} else {
				deploymentInfo.State = DELETED
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
