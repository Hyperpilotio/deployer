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
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/job"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/hyperpilotio/deployer/store"
	logging "github.com/op/go-logging"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"

	"net/http"

	"github.com/aws/aws-sdk-go/aws"
)

var logFormatter = logging.MustStringFormatter(
	` %{level:.1s}%{time:0102 15:04:05.999999} %{pid} %{shortfile}] %{message}`,
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

func getStateString(state DeploymentState) string {
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

type DeploymentInfo struct {
	awsInfo   *awsecs.DeployedCluster
	scheduler *job.Scheduler
	created   time.Time
	state     DeploymentState
}

func (deploymentInfo *DeploymentInfo) getDeploymentType() string {
	if deploymentInfo.awsInfo.Deployment.KubernetesDeployment != nil {
		return "K8S"
	} else {
		return "ECS"
	}
}

// Server store the stats / data of every deployment
type Server struct {
	Config      *viper.Viper
	Store       store.Store
	AWSProfiles map[string]*awsecs.AWSProfile
	// Maps deployment name to deployed cluster struct
	DeployedClusters   map[string]*DeploymentInfo
	KubernetesClusters *kubernetes.KubernetesClusters

	// Maps file id to location on disk
	UploadedFiles map[string]string
	mutex         sync.Mutex
}

// NewServer return an instance of Server struct.
func NewServer(config *viper.Viper) *Server {
	return &Server{
		Config:             config,
		DeployedClusters:   make(map[string]*DeploymentInfo),
		KubernetesClusters: kubernetes.NewKubernetesClusters(),
		UploadedFiles:      make(map[string]string),
	}
}

// NewLogger create per deployment logger
func (server *Server) NewLogger(deploymentInfo *DeploymentInfo, deployedCluster *awsecs.DeployedCluster) error {
	deploymentName := deployedCluster.Deployment.Name
	log := logging.MustGetLogger(deploymentName)

	logDirPath := path.Join(server.Config.GetString("filesPath"), "log")
	if _, err := os.Stat(logDirPath); os.IsNotExist(err) {
		os.Mkdir(logDirPath, 0777)
	}

	logFilePath := path.Join(logDirPath, deploymentName+".log")
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return errors.New("Unable to create deployment log file:" + err.Error())
	}

	fileLog := logging.NewLogBackend(logFile, "["+deploymentName+"]", 0)
	consoleLog := logging.NewLogBackend(os.Stdout, "["+deploymentName+"]", 0)

	fileLogLevel := logging.AddModuleLevel(fileLog)
	fileLogLevel.SetLevel(logging.INFO, "")

	consoleLogBackend := logging.NewBackendFormatter(consoleLog, logFormatter)
	fileLogBackend := logging.NewBackendFormatter(fileLog, logFormatter)

	log.SetBackend(logging.SetBackend(fileLogBackend, consoleLogBackend))

	deploymentInfo.logFile = logFile
	deployedCluster.Logger = log

	return nil
}

// NewStoreDeployment create deployment that needs to be stored
func (server *Server) NewStoreDeployment(deploymentName string) *store.StoreDeployment {
	deploymentInfo := server.DeployedClusters[deploymentName]
	storeDeployment := &store.StoreDeployment{
		Name:    deploymentName,
		Region:  deploymentInfo.awsInfo.Deployment.Region,
		UserId:  deploymentInfo.awsInfo.Deployment.UserId,
		Status:  getStateString(deploymentInfo.state),
		Created: deploymentInfo.created.Format(time.RFC822),
	}

	if deploymentInfo.awsInfo.KeyPair != nil {
		storeDeployment.KeyMaterial = aws.StringValue(deploymentInfo.awsInfo.KeyPair.KeyMaterial)
	}

	k8sDeployment, ok := server.KubernetesClusters.Clusters[deploymentName]
	if ok {
		storeDeployment.K8SDeployment = k8sDeployment.NewK8SStoreDeployment()
		storeDeployment.Type = "K8S"
	} else {
		storeDeployment.Type = "ECS"
		storeDeployment.ECSDeployment = deploymentInfo.awsInfo.NewECSStoreDeployment()
	}

	return storeDeployment
}

// reloadClusterState reload cluster state when deployer restart
func (server *Server) reloadClusterState() error {
	awsProfiles, profileErr := server.Store.LoadAWSProfiles()
	if profileErr != nil {
		return fmt.Errorf("Unable to load aws profile: %s", profileErr.Error())
	}

	awsProfileInfos := map[string]*awsecs.AWSProfile{}
	for _, awsProfile := range awsProfiles {
		awsProfileInfos[awsProfile.UserId] = awsProfile
	}
	server.AWSProfiles = awsProfileInfos

	deployments, err := server.Store.LoadDeployments()
	if err != nil {
		return fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	scheduleRunTime, err := time.ParseDuration(server.Config.GetString("shutDownTime"))
	if err != nil {
		return fmt.Errorf("Unable to parse shutDownTime %s: %s", scheduleRunTime, err.Error())
	}

	for _, storeDeployment := range deployments {
		if storeDeployment.Status == "Deleted" || storeDeployment.Status == "Failed" {
			continue
		}

		userId := storeDeployment.UserId
		if userId == "" {
			glog.Warning("Skip loading deployment with unspecified user id")
			continue
		}

		awsProfile, ok := awsProfileInfos[userId]
		if !ok {
			return fmt.Errorf("Unable to find %s aws profile", userId)
		}

		deploymentName := storeDeployment.Name
		deployment := &apis.Deployment{
			UserId: userId,
			Name:   deploymentName,
			Region: storeDeployment.Region,
		}
		deployedCluster := awsecs.NewDeployedCluster(deployment)
		deploymentInfo := &DeploymentInfo{
			awsInfo: deployedCluster,
			state:   AVAILABLE,
			created: time.Now(),
		}
		if err := server.NewLogger(deploymentInfo, deployedCluster); err != nil {
			glog.Warningf("Skip reloading cluster %s: Unable to reinitialize log: %s", deploymentName, err.Error())
		}

		// Reload keypair
		if err := awsecs.ReloadKeyPair(awsProfile, deployedCluster, storeDeployment.KeyMaterial); err != nil {
			glog.Warningf("Skip reloading because unable to load %s keyPair: %s", deploymentName, err.Error())
			continue
		}

		reloaded := true
		switch storeDeployment.Type {
		case "ECS":
			deployedCluster.Deployment.ECSDeployment = &apis.ECSDeployment{}
			if err := awsecs.ReloadClusterState(awsProfile, deployedCluster); err != nil {
				glog.Warningf("Unable to load %s ECS deployedCluster status: %s", deploymentName, err.Error())
				reloaded = false
			}
		case "K8S":
			if err := kubernetes.CheckClusterState(awsProfile, deployedCluster); err != nil {
				if err := server.Store.DeleteDeployment(deploymentName); err != nil {
					glog.Warningf("Unable to delete %s deployment after failed check: %s", deploymentName, err.Error())
				}
				glog.Warningf("Skipping reloading because unable to load %s stack: %s", deploymentName, err.Error())
				continue
			}

			deployedCluster.Deployment.KubernetesDeployment = &apis.KubernetesDeployment{}
			k8sDeployment, err := kubernetes.ReloadClusterState(storeDeployment.K8SDeployment, deployedCluster)
			if err != nil {
				glog.Warningf("Unable to load %s K8S deployedCluster status: %s", deploymentName, err.Error())
				reloaded = false
			}
			server.KubernetesClusters.Clusters[deploymentName] = k8sDeployment
		default:
			reloaded = false
			glog.Warningf("Unsupported deployment store type: " + storeDeployment.Type)
		}

		if reloaded {
			server.DeployedClusters[deploymentName] = &DeploymentInfo{
				awsInfo: deployedCluster,
				state:   AVAILABLE,
				created: time.Now(),
			}

			// Add auto shutDown cluster schedule
			newScheduleRunTime := ""
			if createdTime, err := time.Parse(time.RFC822, storeDeployment.Created); err == nil {
				realScheduleRunTime := createdTime.Add(scheduleRunTime)
				if realScheduleRunTime.After(time.Now()) {
					newScheduleRunTime = realScheduleRunTime.Sub(time.Now()).String()
				}
			}

			scheduler, scheduleErr := job.NewScheduler(server.Config, deploymentName, newScheduleRunTime,
				server.getShutDownClusterFunc(deploymentName))
			if scheduleErr != nil {
				glog.Warningf("Unable to schedule %s to auto shutdown cluster: %s", deployment.Name, scheduleErr.Error())
			} else {
				server.DeployedClusters[deploymentName].scheduler = scheduler
			}
		}
	}

	return nil
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

	storeSvc, err := store.NewStore(server.Config)
	if err != nil {
		return errors.New("Unable to new store: " + err.Error())
	}
	server.Store = storeSvc

	if err := server.reloadClusterState(); err != nil {
		return errors.New("Unable to reload cluster state: " + err.Error())
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

	daemonsGroup := router.Group("/v1/deployments")
	{
		daemonsGroup.GET("", server.getAllDeployments)
		daemonsGroup.GET("/:deployment", server.getDeployment)
		daemonsGroup.POST("", server.createDeployment)
		daemonsGroup.DELETE("/:deployment", server.deleteDeployment)
		daemonsGroup.PUT("/:deployment", server.updateDeployment)

		daemonsGroup.GET("/:deployment/ssh_key", server.getPemFile)
		daemonsGroup.GET("/:deployment/kubeconfig", server.getKubeConfigFile)

		daemonsGroup.GET("/:deployment/services/:service/url", server.getServiceUrl)
	}

	filesGroup := router.Group("/v1/files")
	{
		filesGroup.GET("", server.getFiles)
		filesGroup.POST("/:fileId", server.uploadFile)
		filesGroup.DELETE("/:fileId", server.deleteFile)
	}

	return router.Run(":" + server.Config.GetString("port"))
}

func (server *Server) storeUploadedFile(fileId string, c *gin.Context) (*string, error) {
	file, _, err := c.Request.FormFile("upload")
	tempFile := "/tmp/" + fileId + ".tmp"
	destination := path.Join(server.Config.GetString("filesPath"), fileId)
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
	fileId := c.Param("fileId")
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if _, ok := server.UploadedFiles[fileId]; ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "File already uploaded",
		})
		return
	}

	file, err := server.storeUploadedFile(fileId, c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  err.Error(),
		})
		return
	}

	server.UploadedFiles[fileId] = *file

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
	var deployment apis.Deployment
	if err := c.BindJSON(&deployment); err != nil {
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

	if deploymentInfo.state != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Deployment is not available",
		})
		return
	}

	awsProfile, ok := server.AWSProfiles[deployment.UserId]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to find aws profile for user: " + deployment.UserId,
		})
		return
	}

	deployment.Name = deploymentName
	deploymentInfo.awsInfo.Deployment = &deployment
	deploymentInfo.state = UPDATING
	server.mutex.Unlock()

	go func() {
		defer func() {
			deploymentInfo.state = AVAILABLE
		}()
		// TODO: Check if it's ECS or kubernetes
		err := server.KubernetesClusters.UpdateDeployment(awsProfile, &deployment, deploymentInfo.awsInfo)
		if err != nil {
			deploymentInfo.awsInfo.Logger.Infof("Error update deployment: " + err.Error())
			return
		}

		server.storeDeploymentStatus(deploymentName)
		deploymentInfo.awsInfo.Logger.Infof("Update deployment successfully!")
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

func createUniqueDeploymentName(familyName string) string {
	randomId := strings.ToUpper(strings.Split(uuid.NewUUID().String(), "-")[0])
	return fmt.Sprintf("%s-%s", familyName, randomId)
}

func (server *Server) createDeployment(c *gin.Context) {
	// FIXME document the structure of deployment in the doc file
	var deployment apis.Deployment
	if err := c.BindJSON(&deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	deployment.Name = createUniqueDeploymentName(deployment.Name)
	deployedCluster := awsecs.NewDeployedCluster(&deployment)

	deploymentInfo := &DeploymentInfo{
		awsInfo: deployedCluster,
		created: time.Now(),
		state:   CREATING,
	}

	awsProfile, err := func() (*awsecs.AWSProfile, error) {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		if _, ok := server.DeployedClusters[deployment.Name]; ok {
			return nil, errors.New("Already deployed")
		}

		awsProfile, ok := server.AWSProfiles[deployment.UserId]
		if !ok {
			return nil, errors.New("Unable to find aws profile for user: " + deployment.UserId)
		}

		if err := server.NewLogger(deploymentInfo, deployedCluster); err != nil {
			return nil, errors.New("Error creating deployment logger:" + err.Error())
		}

		server.DeployedClusters[deployment.Name] = deploymentInfo

		return awsProfile, nil
	}()

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  err.Error(),
		})
		return
	}

	go func() {
		var err error
		switch deploymentInfo.getDeploymentType() {
		case "ECS":
			if err = awsecs.CreateDeployment(awsProfile, server.UploadedFiles, deployedCluster); err != nil {
				deployedCluster.Logger.Infof("Unable to create ECS deployment: " + err.Error())
			} else {
				deployedCluster.Logger.Infof("Create ECS deployment successfully!")
			}
		case "K8S":
			var response *kubernetes.CreateDeploymentResponse
			response, err = server.KubernetesClusters.CreateDeployment(awsProfile, server.UploadedFiles, deployedCluster)
			if err != nil {
				deployedCluster.Logger.Infof("Unable to create Kubernetes deployment: " + err.Error())
			} else {
				resJson, _ := json.Marshal(*response)
				deployedCluster.Logger.Infof(string(resJson))
				deployedCluster.Logger.Infof("Create Kubernetes deployment successfully!")
			}
		default:
			err = errors.New("")
			deployedCluster.Logger.Infof("Unsupported container deployment")
		}

		server.mutex.Lock()
		if err != nil {
			deploymentInfo.state = FAILED
			server.storeDeploymentStatus(deployment.Name)
			delete(server.DeployedClusters, deployment.Name)
		} else {
			deploymentInfo.state = AVAILABLE
			server.storeDeploymentStatus(deployment.Name)

			// Add auto shutDown cluster schedule
			scheduler, scheduleErr := job.NewScheduler(server.Config, deployment.Name, deployment.ShutDownTime,
				server.getShutDownClusterFunc(deployment.Name))
			if scheduleErr != nil {
				glog.Warningf("Unable to schedule %s to auto shutdown cluster: %s", deployment.Name, scheduleErr.Error())
			} else {
				deploymentInfo.scheduler = scheduler
			}
		}
		server.mutex.Unlock()
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "Creating deployment " + deployedCluster.Deployment.Name + "......",
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

	if deploymentInfo.state != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  deploymentName + " is not available to delete",
		})
		return
	}

	awsProfile, ok := server.AWSProfiles[deploymentInfo.awsInfo.Deployment.UserId]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to find aws profile for user: " + deploymentInfo.awsInfo.Deployment.UserId,
		})
		return
	}

	deploymentInfo.state = DELETING
	server.mutex.Unlock()

	go func() {
		var err error
		if deploymentInfo.awsInfo.Deployment.KubernetesDeployment != nil {
			server.KubernetesClusters.DeleteDeployment(awsProfile, deploymentInfo.awsInfo)
		} else {
			err = awsecs.DeleteDeployment(awsProfile, deploymentInfo.awsInfo)
		}

		if err != nil {
			deploymentInfo.state = FAILED
			deploymentInfo.awsInfo.Logger.Error("Unable to delete deployment: " + err.Error())
		} else {
			defer deploymentInfo.scheduler.Stop()
			deploymentInfo.state = DELETED
			deploymentInfo.awsInfo.Logger.Infof("Delete deployment successfully!")
		}
		server.mutex.Lock()
		server.storeDeploymentStatus(deploymentName)
		delete(server.DeployedClusters, deploymentName)
		server.mutex.Unlock()
		deploymentInfo.logFile.Close()
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "Start to delete deployment " + deploymentName + "......",
	})
}

func (server *Server) getPemFile(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if deploymentInfo, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		if deploymentInfo.awsInfo != nil && deploymentInfo.awsInfo.KeyPair != nil {
			privateKey := strings.Replace(*deploymentInfo.awsInfo.KeyPair.KeyMaterial, "\\n", "\n", -1)
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

	if k8sDeployment, ok := server.KubernetesClusters.Clusters[c.Param("deployment")]; ok {
		if k8sDeployment.KubeConfigPath != "" {
			if b, err := ioutil.ReadFile(k8sDeployment.KubeConfigPath); err != nil {
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

	serviceUrl := ""
	var err error
	if k8sDeployment, ok := server.KubernetesClusters.Clusters[deploymentName]; ok {
		serviceUrl, err = k8sDeployment.GetServiceUrl(serviceName)
	} else {
		serviceUrl, err = awsecs.GetServiceUrl(deploymentInfo.awsInfo, serviceName)
	}

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to get service url: " + err.Error(),
		})
		return
	}

	c.String(http.StatusOK, serviceUrl)
}

func (server *Server) storeDeploymentStatus(deploymentName string) error {
	deployment := server.NewStoreDeployment(deploymentName)
	if err := server.Store.StoreNewDeployment(deployment); err != nil {
		return fmt.Errorf("Unable to store %s deployment status: %s", deploymentName, err.Error())
	}

	return nil
}

func (server *Server) getShutDownClusterFunc(deploymentName string) func() {
	return func() {
		httpClient := &http.Client{
			Timeout: time.Second * 10,
		}

		deleteUrl := fmt.Sprintf("http://127.0.0.1:7777/v1/deployments/%s", deploymentName)
		req, err := http.NewRequest("DELETE", deleteUrl, nil)
		if err != nil {
			glog.Warningf("Unable to new delete request: %s", err.Error())
			return
		}

		resp, deleteErr := httpClient.Do(req)
		if deleteErr != nil {
			glog.Warningf("Unable to auto shutdown cluster: %s", deleteErr.Error())
			return
		}

		if resp.StatusCode != 202 {
			responseData, _ := ioutil.ReadAll(resp.Body)
			glog.Warningf("Unable to auto shutdown cluster: %s", string(responseData))
			return
		}
	}
}
