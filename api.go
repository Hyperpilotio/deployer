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
	"github.com/golang/glog"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/common"
	"github.com/hyperpilotio/deployer/kubernetes"
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
	Time   string
	Status string
}

type DeploymentLogs []*DeploymentLog

func (d DeploymentLogs) Len() int { return len(d) }
func (d DeploymentLogs) Less(i, j int) bool {
	t1, _ := time.Parse(time.RFC3339, d[i].Time)
	t2, _ := time.Parse(time.RFC3339, d[j].Time)
	return t1.Before(t2)
}
func (d DeploymentLogs) Swap(i, j int) { d[i], d[j] = d[j], d[i] }

type DeploymentStatus struct {
	DeployedClusters   map[string]*awsecs.DeployedCluster
	KubernetesClusters *kubernetes.KubernetesClusters
}

// Server store the stats / data of every deployment
type Server struct {
	Config *viper.Viper
	// Maps deployment name to deployed cluster struct
	DeployedClusters   map[string]*awsecs.DeployedCluster
	KubernetesClusters *kubernetes.KubernetesClusters

	// Maps file id to location on disk
	UploadedFiles map[string]string
	mutex         sync.Mutex
}

// NewServer return an instance of Server struct.
func NewServer(config *viper.Viper) *Server {
	return &Server{
		Config:             config,
		DeployedClusters:   make(map[string]*awsecs.DeployedCluster),
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

// StartServer start a web server
func (server *Server) StartServer() error {
	if server.Config.GetString("filesPath") == "" {
		return errors.New("filesPath is not specified in the configuration file.")
	}

	if err := os.Mkdir(server.Config.GetString("filesPath"), 0755); err != nil {
		if !os.IsExist(err) {
			return errors.New("Unable to create filesPath directory: " + err.Error())
		}
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
		uiGroup.GET("/:logFile/list", server.getDeploymentLog)
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

		daemonsGroup.POST("/:deployment/task", server.startTask)
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

	deployedCluster, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment",
		})
		return
	}

	nodeId := -1
	for _, nodeMapping := range deployedCluster.Deployment.NodeMapping {
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

	nodeInfo, nodeOk := deployedCluster.NodeInfos[nodeId]
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

	server.mutex.Lock()
	defer server.mutex.Unlock()

	data, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	// TODO Implement function to update deployment
	var deployment apis.Deployment
	if err := c.BindJSON(&deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}
	deploymentName = data.Deployment.Name
	data.Deployment = &deployment
	data.Deployment.Name = deploymentName

	f, logErr := server.NewLogger(data)
	if logErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error creating deployment logger:" + logErr.Error(),
		})
		return
	}
	defer f.Close()

	// TODO: Check if it's ECS or kubernetes
	err := server.KubernetesClusters.UpdateDeployment(server.Config, &deployment, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Error update deployment: " + err.Error(),
		})
	} else {
		server.storeDeploymentStatus()
		c.JSON(http.StatusOK, gin.H{
			"error": false,
			"data":  "",
		})
	}
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

	server.mutex.Lock()
	defer server.mutex.Unlock()

	if _, ok := server.DeployedClusters[deployment.Name]; ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}

	deployedCluster := awsecs.NewDeployedCluster(&deployment)
	server.DeployedClusters[deployment.Name] = deployedCluster

	f, logErr := server.NewLogger(deployedCluster)
	if logErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error creating deployment logger:" + logErr.Error(),
		})
		return
	}
	defer f.Close()

	if deployment.ECSDeployment != nil {
		err := awsecs.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to create ECS deployment: " + err.Error(),
			})
			return
		}
	} else if deployment.KubernetesDeployment != nil {
		info, err := server.KubernetesClusters.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to create Kubernetes deployment: " + err.Error(),
			})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
			"data":  info,
		})
	} else {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Unsupported container deployment",
		})
		return
	}

	// Deployment succeeded, storing deployment status
	server.storeDeploymentStatus()
}

func (server *Server) startTask(c *gin.Context) {
	deploymentName := c.Param("deployment")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deployment, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	var nodeMapping apis.NodeMapping
	if err := c.BindJSON(&nodeMapping); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing nodeMapping: " + err.Error(),
		})
		return
	}

	if err := awsecs.StartTaskOnNode(server.Config, deployment, &nodeMapping); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Unable to start task: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) deleteDeployment(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if data, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		f, logErr := server.NewLogger(data)
		if logErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Error creating deployment logger:" + logErr.Error(),
			})
			return
		}
		defer f.Close()

		// TODO create a batch job to delete the deployment
		if data.Deployment.KubernetesDeployment != nil {
			server.KubernetesClusters.DeleteDeployment(server.Config, data)
		} else {
			awsecs.DeleteDeployment(server.Config, data)
		}

		delete(server.DeployedClusters, c.Param("deployment"))
		server.storeDeploymentStatus()

		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
			"data":  "",
		})
	} else {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
	}
}

func (server *Server) getPemFile(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if data, ok := server.DeployedClusters[c.Param("deployment")]; ok {
		privateKey := strings.Replace(*data.KeyPair.KeyMaterial, "\\n", "\n", -1)
		c.String(http.StatusOK, privateKey)
	} else {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
	}
}

func (server *Server) getKubeConfigFile(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if data, ok := server.KubernetesClusters.Clusters[c.Param("deployment")]; ok {
		if b, err := ioutil.ReadFile(data.KubeConfigPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": true,
				"data":  "Unable to read kubeConfig file: " + err.Error(),
			})
		} else {
			c.String(http.StatusOK, string(b))
		}
	} else {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
	}
}

func (server *Server) getContainerUrl(c *gin.Context) {
	deploymentName := c.Param("deployment")
	containerName := c.Param("container")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	deployedCluster, ok := server.DeployedClusters[deploymentName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find deployment",
		})
		return
	}

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
	logPath := path.Join(server.Config.GetString("filesPath"), "log")
	if files, err := ioutil.ReadDir(logPath); err != nil {
		c.HTML(http.StatusNotFound, "index.html", gin.H{
			"msg":  "Unable to read deployment log:" + err.Error(),
			"logs": "",
		})
	} else {
		deploymentLogs := DeploymentLogs{}
		for _, f := range files {
			// TODO deployment status: deployed, error
			deploymentLog := &DeploymentLog{
				Name: f.Name(),
				Time: f.ModTime().Format(time.RFC3339),
			}
			deploymentLogs = append(deploymentLogs, deploymentLog)
		}

		sort.Sort(deploymentLogs)

		c.HTML(http.StatusOK, "index.html", gin.H{
			"msg":  "Hello hyperpilot!",
			"logs": deploymentLogs,
		})
	}
}

func (server *Server) getDeploymentLog(c *gin.Context) {
	logFile := c.Param("logFile")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	logPath := path.Join(server.Config.GetString("filesPath"), "log", logFile)
	file, err := os.Open(logPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to read deployment log",
		})
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	lines := []string{}
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  lines,
	})
}

func (server *Server) storeDeploymentStatus() {
	deploymentStatus := &DeploymentStatus{
		DeployedClusters:   server.DeployedClusters,
		KubernetesClusters: server.KubernetesClusters,
	}

	depStatPath := path.Join(server.Config.GetString("filesPath"), "DeploymentStatus")
	if err := common.Store(depStatPath, &deploymentStatus); err != nil {
		glog.Warningf("Unable to store deployment status: %s", err.Error())
	}
}

func (server *Server) loadDeploymentStatus() {
	deploymentStatus := &DeploymentStatus{}
	depStatPath := path.Join(server.Config.GetString("filesPath"), "DeploymentStatus")
	if _, err := os.Stat(depStatPath); err == nil {
		if err := common.Load(depStatPath, &deploymentStatus); err != nil {
			glog.Warningf("Unable to read deploymentStatus status file: %s", err.Error())
		} else {
			server.DeployedClusters = deploymentStatus.DeployedClusters
			server.KubernetesClusters = deploymentStatus.KubernetesClusters
			if len(server.DeployedClusters) > 0 {
				for deploymentName := range server.DeployedClusters {
					glog.Infof("Find deployment name %s in deployedClusters...", deploymentName)
				}
			}
		}
	}
}
