package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/hyperpilotio/deployer/store"
	logging "github.com/op/go-logging"
	"github.com/pborman/uuid"
	"github.com/spf13/viper"

	"net/http"
)

var logFormatter = logging.MustStringFormatter(
	` %{level:.1s}%{time:0102 15:04:05.999999} %{pid} %{shortfile}] %{message}`,
)

type DeploymentLog struct {
	Name   string
	Time   time.Time
	Type   string
	Status string
}

type DeploymentLogs []*DeploymentLog

func (d DeploymentLogs) Len() int { return len(d) }
func (d DeploymentLogs) Less(i, j int) bool {
	return d[i].Time.Before(d[j].Time)
}
func (d DeploymentLogs) Swap(i, j int) { d[i], d[j] = d[j], d[i] }

type DeploymentState int

// Possible deployment states
const (
	AVAILABLE = 0
	CREATING  = 1
	UPDATING  = 2
	DELETING  = 3
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
	}

	return ""
}

type DeploymentInfo struct {
	awsInfo *awsecs.DeployedCluster
	created time.Time
	state   DeploymentState
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
	Config *viper.Viper
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
func (server *Server) NewLogger(deployedCluster *awsecs.DeployedCluster) (*os.File, error) {
	deploymentName := deployedCluster.Deployment.Name
	log := logging.MustGetLogger(deploymentName)

	logDirPath := path.Join(server.Config.GetString("filesPath"), "log")
	if _, err := os.Stat(logDirPath); os.IsNotExist(err) {
		os.Mkdir(logDirPath, 0777)
	}

	logFilePath := path.Join(logDirPath, deploymentName+".log")
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return logFile, errors.New("Unable to create deployment log file:" + err.Error())
	}

	fileLog := logging.NewLogBackend(logFile, "["+deploymentName+"]", 0)
	consoleLog := logging.NewLogBackend(os.Stdout, "["+deploymentName+"]", 0)

	fileLogLevel := logging.AddModuleLevel(fileLog)
	fileLogLevel.SetLevel(logging.INFO, "")

	consoleLogBackend := logging.NewBackendFormatter(consoleLog, logFormatter)
	fileLogBackend := logging.NewBackendFormatter(fileLog, logFormatter)

	logging.SetBackend(fileLogBackend, consoleLogBackend)

	deployedCluster.Logger = log

	return logFile, nil
}

// NewStoreDeployment create deployment that needs to be stored
func (server *Server) NewStoreDeployment(deploymentName string) *store.StoreDeployment {
	deploymentInfo := server.DeployedClusters[deploymentName]
	storeDeployment := &store.StoreDeployment{
		Name:   deploymentName,
		Region: deploymentInfo.awsInfo.Deployment.Region,
		Status: getStateString(deploymentInfo.state),
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
	storeSvc, err := store.NewStore(server.Config)
	if err != nil {
		return fmt.Errorf("Unable to new store: %s", err.Error())
	}

	deployments, err := storeSvc.LoadDeployments()
	if err != nil {
		return fmt.Errorf("Unable to load deployment status: %s", err.Error())
	}

	for _, storeDeployment := range deployments {
		deploymentName := storeDeployment.Name
		deployment := &apis.Deployment{
			Name:   deploymentName,
			Region: storeDeployment.Region,
		}
		deployedCluster := awsecs.NewDeployedCluster(deployment)

		// Reload keypair
		if err := awsecs.ReloadKeyPair(server.Config, deployedCluster, storeDeployment.KeyMaterial); err != nil {
			return fmt.Errorf("Unable to load %s keyPair: %s", deploymentName, err.Error())
		}

		switch storeDeployment.Type {
		case "ECS":
			deployedCluster.Deployment.ECSDeployment = &apis.ECSDeployment{}
			if err := awsecs.ReloadClusterState(server.Config, deployedCluster); err != nil {
				return fmt.Errorf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
			}
		case "K8S":
			deployedCluster.Deployment.KubernetesDeployment = &apis.KubernetesDeployment{}
			k8sDeployment, err := kubernetes.ReloadClusterState(storeDeployment.K8SDeployment, deployedCluster)
			if err != nil {
				return fmt.Errorf("Unable to load %s deployedCluster status: %s", deploymentName, err.Error())
			}
			server.KubernetesClusters.Clusters[deploymentName] = k8sDeployment
		default:
			return errors.New("Unsupported deployment store type: " + storeDeployment.Type)
		}

		server.DeployedClusters[deploymentName] = &DeploymentInfo{
			awsInfo: deployedCluster,
			state:   AVAILABLE,
			created: time.Now(),
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

	if err := server.reloadClusterState(); err != nil {
		return errors.New("Unable to reload cluster state: " + err.Error())
	}

	//gin.SetMode("release")
	router := gin.New()

	// Global middleware
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	router.LoadHTMLGlob("ui/*.html")
	router.Static("/static", "./ui/static")

	uiGroup := router.Group("/ui")
	{
		uiGroup.GET("", server.logUI)
		uiGroup.GET("/refresh", server.refreshUI)
		uiGroup.GET("/list/:logFile", server.getDeploymentLog)
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

		daemonsGroup.GET("/:deployment/tasks/:task/node_address", server.getNodeAddressForTask)
		daemonsGroup.GET("/:deployment/containers/:container/url", server.getContainerUrl)
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
	for _, nodeMapping := range deploymentInfo.awsInfo.Deployment.NodeMapping {
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

	nodeInfo, nodeOk := deploymentInfo.awsInfo.NodeInfos[nodeId]
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

	deployment.Name = deploymentName
	deploymentInfo.awsInfo.Deployment = &deployment
	deploymentInfo.state = UPDATING
	server.mutex.Unlock()

	defer func() {
		deploymentInfo.state = AVAILABLE
	}()

	f, logErr := server.NewLogger(deploymentInfo.awsInfo)
	if logErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error creating deployment logger:" + logErr.Error(),
		})
		return
	}
	defer f.Close()

	// TODO: Check if it's ECS or kubernetes
	err := server.KubernetesClusters.UpdateDeployment(server.Config, &deployment, deploymentInfo.awsInfo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Error update deployment: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	server.storeDeploymentStatus(deploymentName)
	server.mutex.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
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

	f, logErr := server.NewLogger(deployedCluster)
	if logErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error creating deployment logger:" + logErr.Error(),
		})
		return
	}
	defer f.Close()

	server.mutex.Lock()
	if _, ok := server.DeployedClusters[deployment.Name]; ok {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}

	deploymentInfo := &DeploymentInfo{
		awsInfo: deployedCluster,
		state:   CREATING,
	}
	server.DeployedClusters[deployment.Name] = deploymentInfo
	server.mutex.Unlock()

	switch deploymentInfo.getDeploymentType() {
	case "ECS":
		if err := awsecs.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster); err != nil {
			server.mutex.Lock()
			delete(server.DeployedClusters, deployment.Name)
			server.mutex.Unlock()

			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to create ECS deployment: " + err.Error(),
			})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
		})
	case "K8S":
		response, err := server.KubernetesClusters.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster)
		if err != nil {
			server.mutex.Lock()
			delete(server.DeployedClusters, deployment.Name)
			server.mutex.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to create Kubernetes deployment: " + err.Error(),
			})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
			"data":  response,
		})
	default:
		server.mutex.Lock()
		delete(server.DeployedClusters, deployment.Name)
		server.mutex.Unlock()

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Unsupported container deployment",
		})
		return
	}

	// Deployment succeeded, storing deployment status
	server.mutex.Lock()
	server.storeDeploymentStatus(deployment.Name)
	server.mutex.Unlock()

	deploymentInfo.state = AVAILABLE
}

func (server *Server) deleteDeployment(c *gin.Context) {
	server.mutex.Lock()

	deploymentInfo, ok := server.DeployedClusters[c.Param("deployment")]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
		return
	}

	if deploymentInfo.state != AVAILABLE {
		server.mutex.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " is not available to delete",
		})
		return
	}

	deploymentInfo.state = DELETING
	server.mutex.Unlock()

	defer func() {
		deploymentInfo.state = AVAILABLE
	}()

	f, logErr := server.NewLogger(deploymentInfo.awsInfo)
	if logErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error creating deployment logger:" + logErr.Error(),
		})
		return
	}
	defer f.Close()

	if deploymentInfo.awsInfo.Deployment.KubernetesDeployment != nil {
		server.KubernetesClusters.DeleteDeployment(server.Config, deploymentInfo.awsInfo)
	} else {
		awsecs.DeleteDeployment(server.Config, deploymentInfo.awsInfo)
	}

	server.mutex.Lock()
	server.storeDeploymentStatus(deploymentInfo.awsInfo.Deployment.Name)
	delete(server.DeployedClusters, c.Param("deployment"))
	server.mutex.Unlock()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
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

func (server *Server) getContainerUrl(c *gin.Context) {
	deploymentName := c.Param("deployment")
	containerName := c.Param("container")

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

	deployedCluster := deploymentInfo.awsInfo

	nodePort := ""
	taskFamilyName := ""
	for _, task := range deployedCluster.Deployment.TaskDefinitions {
		for _, container := range task.ContainerDefinitions {
			if *container.Name == containerName {
				nodePort = strconv.FormatInt(*container.PortMappings[0].HostPort, 10)
				taskFamilyName = *task.Family
				break
			}
		}
	}

	if nodePort == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find container in deployment container defintiions",
		})
		return
	}

	nodeId := -1
	for _, nodeMapping := range deployedCluster.Deployment.NodeMapping {
		if nodeMapping.Task == taskFamilyName {
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

	nodeInfo, nodeOk := deployedCluster.NodeInfos[nodeId]
	if !nodeOk {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find node in cluster",
		})
		return
	}

	c.String(http.StatusOK, nodeInfo.PublicDnsName+":"+nodePort)
}

func (server *Server) logUI(c *gin.Context) {
	deploymentLogs := server.getDeploymentLogs(c)
	c.HTML(http.StatusOK, "index.html", gin.H{
		"logs": deploymentLogs,
	})
}

func (server *Server) refreshUI(c *gin.Context) {
	deploymentLogs := server.getDeploymentLogs(c)
	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  deploymentLogs,
	})
}

func (server *Server) getDeploymentLogs(c *gin.Context) DeploymentLogs {
	deploymentLogs := DeploymentLogs{}

	server.mutex.Lock()
	defer server.mutex.Unlock()

	for name, deploymentInfo := range server.DeployedClusters {
		deploymentLog := &DeploymentLog{
			Name:   name,
			Time:   deploymentInfo.created,
			Type:   deploymentInfo.getDeploymentType(),
			Status: getStateString(deploymentInfo.state),
		}
		deploymentLogs = append(deploymentLogs, deploymentLog)
	}

	sort.Sort(deploymentLogs)
	return deploymentLogs
}

func (server *Server) getDeploymentLog(c *gin.Context) {
	logFile := c.Param("logFile")
	logPath := path.Join(server.Config.GetString("filesPath"), "log", logFile)
	file, err := os.Open(logPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to read deployment log: " + err.Error(),
		})
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	lines := []string{}
	// TODO: Find a way to pass io.reader to repsonse directly, to avoid copying
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  lines,
	})
}

func (server *Server) storeDeploymentStatus(deploymentName string) error {
	storeSvc, err := store.NewStore(server.Config)
	if err != nil {
		return fmt.Errorf("Unable to new store: %s", err.Error())
	}

	deployment := server.NewStoreDeployment(deploymentName)
	if err := storeSvc.StoreNewDeployment(deployment); err != nil {
		return fmt.Errorf("Unable to store %s deployment status: %s", deploymentName, err.Error())
	}

	return nil
}
