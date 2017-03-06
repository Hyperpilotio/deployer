package main

import (
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/hyperpilotio/deployer/kubernetes"
	"github.com/spf13/viper"

	"net/http"
)

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
	//deploymentName := c.Param("deployment")

	server.mutex.Lock()
	defer server.mutex.Unlock()

	/*
		data, ok := server.DeployedClusters[deploymentName]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{
				"error": true,
				"data":  "Deployment not found",
			})
			return
		}
	**/

	// TODO Implement function to update deployment
	var deployment apis.Deployment
	if err := c.BindJSON(&deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	data := awsecs.NewDeployedCluster(&deployment)

	err := server.KubernetesClusters.UpdateDeployment(server.Config, &deployment, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Error update deployment: " + err.Error(),
		})
	} else {
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

	server.mutex.Lock()
	defer server.mutex.Unlock()

	if _, ok := server.DeployedClusters[deployment.Name]; ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Already deployed",
		})
		return
	}

	var err error
	deployedCluster := awsecs.NewDeployedCluster(&deployment)

	server.DeployedClusters[deployment.Name] = deployedCluster

	if deployment.ECSDeployment != nil {
		err = awsecs.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster)
	} else if deployment.KubernetesDeployment != nil {
		err = server.KubernetesClusters.CreateDeployment(server.Config, server.UploadedFiles, deployedCluster)

	} else {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Unsupported container deployment",
		})
		return
	}

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to create deployment: " + err.Error(),
		})
		return
	}

	//server.DeployedClusters[deployment.Name] = deployedCluster

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})

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
		// TODO create a batch job to delete the deployment
		// TODO Delete deployment depending on k8s or awsecs
		//awsecs.DeleteDeployment(server.Config, data)
		server.KubernetesClusters.DeleteDeployment(server.Config, data)

		// NOTE if deployment failed, keep the data in the server.DeployedClusters map
		// delete(server.DeployedClusters, c.Param("deployment"))
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
