package main

import (
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
	"github.com/hyperpilotio/deployer/deploy"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/hyperpilotio/deployer/store"
	"github.com/spf13/viper"

	"net/http"
)

// Server store the stats / data of every deployment
type Server struct {
	Config      *viper.Viper
	Store       store.Store
	AWSProfiles map[string]*awsecs.AWSProfile
	Deployer    map[string]deploy.Deployer
	// Maps deployment name to deployed cluster struct
	DeployedClusters map[string]*awsecs.DeploymentInfo

	// Maps file id to location on disk
	UploadedFiles map[string]string
	mutex         sync.Mutex
}

// NewServer return an instance of Server struct.
func NewServer(config *viper.Viper) *Server {
	return &Server{
		Config:           config,
		Deployer:         make(map[string]deploy.Deployer),
		DeployedClusters: make(map[string]*awsecs.DeploymentInfo),
		UploadedFiles:    make(map[string]string),
	}
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
	for _, nodeMapping := range deploymentInfo.AwsInfo.Deployment.NodeMapping {
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

	nodeInfo, nodeOk := deploymentInfo.AwsInfo.NodeInfos[nodeId]
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

	deployer, ok := server.Deployer[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  fmt.Sprintf("Error initialize %s deployer", deploymentName),
		})
		return
	}
	server.mutex.Unlock()

	deploymentInfo := deployer.GetDeploymentInfo()
	if deploymentInfo.State != awsecs.AVAILABLE {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  deploymentName + " is not available to update",
		})
		return
	}
	deploymentInfo.State = awsecs.UPDATING

	go func() {
		log := deployer.GetLog()
		defer log.LogFile.Close()

		if err := deployer.UpdateDeployment(); err != nil {
			log.Logger.Error("Unable to update deployment")
			deploymentInfo.State = awsecs.FAILED
		} else {
			log.Logger.Infof("Update deployment successfully!")
			deploymentInfo.State = awsecs.AVAILABLE
		}
		server.storeDeploymentStatus(deploymentInfo)
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

	deployer, err := deploy.NewDeployer(server.Config, server.AWSProfiles, &deployment, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error initialize deployer: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	if _, ok := server.DeployedClusters[deployment.Name]; ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}
	server.mutex.Unlock()

	go func() {
		log := deployer.GetLog()
		defer log.LogFile.Close()

		deploymentInfo := deployer.GetDeploymentInfo()
		if err := deployer.CreateDeployment(server.UploadedFiles); err != nil {
			log.Logger.Infof("Unable to create deployment: " + err.Error())
			deploymentInfo.State = awsecs.FAILED
		} else {
			log.Logger.Infof("Create deployment successfully!")
			deploymentInfo.Created = time.Now()
			deploymentInfo.State = awsecs.AVAILABLE

			server.mutex.Lock()
			server.DeployedClusters[deployment.Name] = deploymentInfo
			server.Deployer[deployment.Name] = deployer
			server.mutex.Unlock()
		}
		server.storeDeploymentStatus(deploymentInfo)

		if deploymentInfo.State == awsecs.AVAILABLE {
			if err := deployer.NewShutDownScheduler(""); err != nil {
				glog.Warningf("Unable to New  %s auto shutdown scheduler", deployment.Name)
			}
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "Creating deployment " + deployment.Name + "......",
	})
}

func (server *Server) deleteDeployment(c *gin.Context) {
	deploymentName := c.Param("deployment")

	server.mutex.Lock()
	deployer, ok := server.Deployer[deploymentName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  fmt.Sprintf("Error initialize %s deployer", deploymentName),
		})
		return
	}
	server.mutex.Unlock()

	deploymentInfo := deployer.GetDeploymentInfo()
	if deploymentInfo.State != awsecs.AVAILABLE {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  deploymentName + " is not available to delete",
		})
		return
	}

	scheduler := deployer.GetScheduler()
	if scheduler != nil {
		scheduler.Stop()
	}
	deploymentInfo.State = awsecs.DELETING

	go func() {
		log := deployer.GetLog()
		defer log.LogFile.Close()

		if err := deployer.DeleteDeployment(); err != nil {
			log.Logger.Error("Unable to delete deployment")
			deploymentInfo.State = awsecs.FAILED
		} else {
			log.Logger.Infof("Delete deployment successfully!")
			deploymentInfo.State = awsecs.DELETED
		}
		server.storeDeploymentStatus(deploymentInfo)

		server.mutex.Lock()
		delete(server.DeployedClusters, deploymentName)
		delete(server.Deployer, deploymentName)
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
		if deploymentInfo.AwsInfo != nil && deploymentInfo.AwsInfo.KeyPair != nil {
			privateKey := strings.Replace(*deploymentInfo.AwsInfo.KeyPair.KeyMaterial, "\\n", "\n", -1)
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

	if deployer, ok := server.Deployer[c.Param("deployment")]; ok {
		kubeConfigPath := deployer.GetKubeConfigPath()
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

func (server *Server) storeDeploymentStatus(deploymentInfo *awsecs.DeploymentInfo) error {
	deployment := deploymentInfo.NewStoreDeployment()
	if err := server.Store.StoreNewDeployment(deployment); err != nil {
		return fmt.Errorf("Unable to store %s deployment status: %s",
			deploymentInfo.AwsInfo.Deployment.Name, err.Error())
	}

	return nil
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

		deploymentName := storeDeployment.Name
		deployment := &apis.Deployment{
			UserId: userId,
			Name:   deploymentName,
			Region: storeDeployment.Region,
		}

		deployer, err := deploy.NewDeployer(server.Config, awsProfileInfos, deployment, true)
		if err != nil {
			return fmt.Errorf("Error initialize %s deployer %s", deploymentName, err.Error())
		}

		deploymentInfo := deployer.GetDeploymentInfo()

		// Reload keypair
		if err := deploymentInfo.ReloadKeyPair(storeDeployment.KeyMaterial); err != nil {
			glog.Warningf("Skipping reloading because unable to load %s keyPair: %s", deploymentName, err.Error())
			continue
		}

		reloaded := true
		switch storeDeployment.Type {
		case "ECS":
			if err := deploymentInfo.ReloadClusterState(); err != nil {
				glog.Warningf("Unable to load %s ECS deployedCluster status: %s", deploymentName, err.Error())
				reloaded = false
			}
		case "K8S":
			if err := kubernetes.CheckClusterState(deploymentInfo); err != nil {
				if err := server.Store.DeleteDeployment(deploymentName); err != nil {
					glog.Warningf("Unable to delete %s deployment after failed check: %s", deploymentName, err.Error())
				}
				glog.Warningf("Skipping reloading because unable to load %s stack: %s", deploymentName, err.Error())
				continue
			}

			if err := kubernetes.ReloadClusterState(deploymentInfo, storeDeployment.K8SDeployment); err != nil {
				glog.Warningf("Unable to load %s K8S deployedCluster status: %s", deploymentName, err.Error())
				reloaded = false
			}
		default:
			reloaded = false
			glog.Warningf("Unsupported deployment store type: " + storeDeployment.Type)
		}

		if reloaded {
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
			server.Deployer[deploymentName] = deployer

			if err := deployer.NewShutDownScheduler(newScheduleRunTime); err != nil {
				glog.Warningf("Unable to New  %s auto shutdown scheduler", deployment.Name)
			}
		}
	}

	return nil
}
