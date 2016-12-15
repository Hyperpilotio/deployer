package main

import (
	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/aws-ecs"
)

// StartServer start a web server
func StartServer(port string) error {
	//gin.SetMode(mode)

	router := gin.New()

	// Global middleware
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	daemonsGroup := router.Group("/deployment")
	{
		daemonsGroup.GET("", getDeployment)
		daemonsGroup.POST("", createDeployment)
		daemonsGroup.DELETE("", deleteDeployment)
	}

	return router.Run(":" + port)
}

func getDeployment(c *gin.Context) {
	// TODO Implement function to get current deployment

	c.JSON(http.StatusNotFound, gin.H{
		"error": false,
		"data":  "",
	})
}

func createDeployment(c *gin.Context) {
	var deployment Deployment
	if c.BindJSON(&deployment) == nil {

		c.JSON(http.StatusAccepted, gin.H{
			"error": false,
			"data":  "",
		})

	}

}

func deleteDeployment(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{
		"error": false,
		"data":  "",
	})
}
