package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/aws"
	"github.com/hyperpilotio/deployer/clustermanagers"
	"github.com/hyperpilotio/go-utils/log"
)

type clusterState int

type cluster struct {
	deploymentId  string
	deployment    *apis.Deployment
	deploymentLog *log.FileLog
	NodeInfos     map[int]*hpaws.NodeInfo
	state         clusterState
	created       time.Time
}

type Clusters struct {
	mutex       sync.Mutex
	Deployments []*cluster
}

func (server *Server) createClusterDeployment(c *gin.Context) {
	clusterName := c.Param("cluster")

	deployment := &apis.Deployment{}
	if err := c.BindJSON(deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[clusterName]
	if !ok {
		server.mutex.Unlock()
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Cluster not found",
		})
		return
	}
	deployment.UserId = deploymentInfo.Deployment.UserId
	deployment.Name = clustermanagers.CreateUniqueDeploymentName(deployment.Name)
	server.mutex.Unlock()

	cluster, err := deploymentInfo.Deployer.GetInternalCluster(server.Config.GetString("filesPath"), deployment)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to create cluster",
		})
		return
	}

	// cluster.State = DEPLOYING

	go func() {
		// log := cluster.deploymentLog

		if _, err := deploymentInfo.Deployer.CreateClusterDeployment(server.UploadedFiles, cluster); err != nil {
			// log.Logger.Error("Unable to create cluster deployment")
			// cluster.state = FAILED
		} else {
			// log.Logger.Infof("Update deployment successfully!")
			// cluster.state = AVAILABLE

			server.mutex.Lock()
			deploymentInfo.Clusters[deployment.Name] = cluster
			server.mutex.Unlock()
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) updateClusterDeployment(c *gin.Context) {

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) deleteClusterDeployment(c *gin.Context) {

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})
}
