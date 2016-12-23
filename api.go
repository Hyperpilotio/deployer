package main

import (
	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/awsecs"
	"github.com/spf13/viper"

	"net/http"
	"sync"
)

// Server store the stats / data of every deployment
type Server struct {
	Config           *viper.Viper
	DeployedClusters map[string]*awsecs.DeployedCluster
	mutex            sync.Mutex
}

// NewServer return an instance of Server struct.
func NewServer(config *viper.Viper) *Server {
	return &Server{
		Config:           config,
		DeployedClusters: make(map[string]*awsecs.DeployedCluster),
	}
}

// StartServer start a web server
func (server *Server) StartServer() error {
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
	}

	return router.Run(":" + server.Config.GetString("port"))
}

func (server *Server) updateDeployment(c *gin.Context) {
	// TODO Implement function to update deployment

	c.JSON(http.StatusNotFound, gin.H{
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

func (server *Server) createDeployment(c *gin.Context) {
	// FIXME document the structure of deployment in the doc file
	var deployment awsecs.Deployment
	if err := c.BindJSON(&deployment); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Error deserializing deployment: " + string(err.Error()),
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

	deployedCluster, err := awsecs.CreateDeployment(server.Config, &deployment)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to create deployment: " + string(err.Error()),
		})
		return
	}
	server.DeployedClusters[deployment.Name] = deployedCluster

	c.JSON(http.StatusAccepted, gin.H{
		"error": false,
		"data":  "",
	})

}

func (server *Server) deleteDeployment(c *gin.Context) {
	if data, ok := server.DeployedClusters[c.Param("deployment")]; ok {

		// TODO create a batch job to delete the deployment
		awsecs.DeleteDeployment(data)

		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
			"data":  "Deleting " + *data.Name,
		})
	} else {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  c.Param("deployment") + " not found.",
		})
	}
}
