package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/apis"
	"github.com/hyperpilotio/deployer/clustermanagers"
)

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

	inCluster, err := clustermanagers.NewInCluster(deploymentInfo.GetDeploymentType(),
		server.Config.GetString("filesPath"), deployment)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to create internal cluster",
		})
		return
	}

	inClusterDeploymentInfo := &DeploymentInfo{
		Deployment: deployment,
		InCluster:  make(map[string]clustermanagers.InCluster),
		Created:    time.Now(),
		State:      CREATING,
	}

	go func() {
		log := inCluster.GetLog()

		if _, err := deploymentInfo.Deployer.CreateInClusterDeployment(server.UploadedFiles, inCluster); err != nil {
			log.Logger.Error("Unable to create cluster deployment")
			inClusterDeploymentInfo.State = FAILED
		} else {
			log.Logger.Infof("Update deployment successfully!")
			inClusterDeploymentInfo.State = AVAILABLE

			server.mutex.Lock()
			inClusterDeploymentInfo.InCluster[deployment.Name] = inCluster
			server.DeployedInClusters[clusterName] = inClusterDeploymentInfo
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
