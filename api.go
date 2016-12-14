package deployer

import (
	"github.com/gin-gonic/gin"
)

// StartServer start a web server
func StartServer(port string) error {
	//gin.SetMode(mode)

	router := gin.New()

	// Global middleware
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	/*daemonsGroup := router.Group("/daemons")
	{
		daemonsGroup.GET("", getDaemonHandler)
		daemonsGroup.POST("", postDaemonHandler)
		daemonsGroup.DELETE(":taskARN", deleteDaemonHandler)
	}
	*/

	return router.Run(":" + port)
}
