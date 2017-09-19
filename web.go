package main

import (
	"bufio"
	"errors"
	"os"
	"path"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/clustermanagers/awsecs"
	"github.com/hyperpilotio/deployer/clustermanagers/kubernetes"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"

	"net/http"
)

type DeploymentLog struct {
	Name     string
	Create   time.Time
	ShutDown time.Time
	Type     string
	Status   string
	UserId   string
}

type DeploymentLogs []*DeploymentLog

func (d DeploymentLogs) Len() int { return len(d) }
func (d DeploymentLogs) Less(i, j int) bool {
	return d[i].Create.Before(d[j].Create)
}
func (d DeploymentLogs) Swap(i, j int) { d[i], d[j] = d[j], d[i] }

func (server *Server) logUI(c *gin.Context) {
	deploymentLogs, _ := server.getDeploymentLogs(c)
	c.HTML(http.StatusOK, "index.html", gin.H{
		"msg":   "Hyperpilot Deployments!",
		"logs":  deploymentLogs,
		"users": server.getDeploymentUsers(c),
	})
}

func (server *Server) refreshUI(c *gin.Context) {
	deploymentLogs, err := server.getDeploymentLogs(c)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"error": true,
			"data":  "",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  deploymentLogs,
	})
}

func (server *Server) userUI(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	c.HTML(http.StatusOK, "user.html", gin.H{
		"error":       false,
		"awsProfiles": server.AWSProfiles,
	})
}

func (server *Server) clusterUI(c *gin.Context) {
	deploymentLogs, _ := server.getDeploymentLogs(c)
	c.HTML(http.StatusOK, "cluster.html", gin.H{
		"msg":  "Hyperpilot Clusters!",
		"logs": deploymentLogs,
	})
}

func (server *Server) getDeploymentLogContent(c *gin.Context) {
	logFile := c.Param("logFile")
	logPath := path.Join(server.Config.GetString("filesPath"), "log", logFile+".log")
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

func (server *Server) getDeploymentLogs(c *gin.Context) (DeploymentLogs, error) {
	deploymentLogs := DeploymentLogs{}

	server.mutex.Lock()
	defer server.mutex.Unlock()

	filterDeploymentStatus := c.Param("status")
	for name, deploymentInfo := range server.DeployedClusters {
		deploymentLog := &DeploymentLog{
			Name:     name,
			Create:   deploymentInfo.Created,
			ShutDown: deploymentInfo.ShutDown,
			Type:     deploymentInfo.GetDeploymentType(),
			Status:   GetStateString(deploymentInfo.State),
			UserId:   deploymentInfo.Deployment.UserId,
		}

		if filterDeploymentStatus == "Failed" {
			// Failed tab only show seployment status is failed
			if deploymentLog.Status == filterDeploymentStatus {
				deploymentLogs = append(deploymentLogs, deploymentLog)
			}
		} else {
			// Running tab show Available, Creating, Updating, Deleting
			if deploymentLog.Status != "Failed" {
				deploymentLogs = append(deploymentLogs, deploymentLog)
			}
		}
	}

	userDeploymentLogs := DeploymentLogs{}
	userId, _ := c.GetQuery("userId")
	if userId != "" {
		for _, deploymentLog := range deploymentLogs {
			if deploymentLog.UserId == userId {
				userDeploymentLogs = append(userDeploymentLogs, deploymentLog)
			}
		}
	} else {
		userDeploymentLogs = deploymentLogs
	}

	sort.Sort(userDeploymentLogs)
	return userDeploymentLogs, nil
}

func (server *Server) getDeploymentUsers(c *gin.Context) []string {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	userIds := []string{}
	for _, awsProfile := range server.AWSProfiles {
		userIds = append(userIds, awsProfile.UserId)
	}
	return userIds
}

func (server *Server) storeUser(c *gin.Context) {
	awsProfile, err := getAWSProfileParam(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to get aws profile param: " + err.Error(),
		})
		return
	}

	if err := server.ProfileStore.Store(awsProfile.UserId, awsProfile); err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to store user data: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	server.AWSProfiles[awsProfile.UserId] = awsProfile
	server.mutex.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) getUser(c *gin.Context) {
	userId := c.Param("userId")
	if userId == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find userId param",
		})
		return
	}

	awsProfile, ok := server.AWSProfiles[userId]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find user data",
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  awsProfile,
	})
}

func (server *Server) deleteUser(c *gin.Context) {
	userId := c.Param("userId")
	if userId == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find userId when deleting",
		})
		return
	}

	if err := server.ProfileStore.Delete(userId); err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to delete user data: " + err.Error(),
		})
		return
	}

	server.mutex.Lock()
	delete(server.AWSProfiles, userId)
	server.mutex.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
	})
}

func getAWSProfileParam(c *gin.Context) (*hpaws.AWSProfile, error) {
	userId, ok := c.GetPostForm("userId")
	if !ok {
		return nil, errors.New("Unable to read userId value")
	}

	awsId, ok := c.GetPostForm("awsId")
	if !ok {
		return nil, errors.New("Unable to read awsId value")
	}

	awsSecret, ok := c.GetPostForm("awsSecret")
	if !ok {
		return nil, errors.New("Unable to read awsSecret value")
	}

	awsProfile := &hpaws.AWSProfile{
		UserId:    userId,
		AwsId:     awsId,
		AwsSecret: awsSecret,
	}

	return awsProfile, nil
}

func (server *Server) getCluster(c *gin.Context) {
	deploymentType := c.Param("deploymentType")
	if deploymentType == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to find deploymentType param",
		})
		return
	}

	clusterName := c.Param("clusterName")
	if clusterName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unable to find clusterName param",
		})
		return
	}

	server.mutex.Lock()
	deploymentInfo, ok := server.DeployedClusters[clusterName]
	server.mutex.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Deployment not found",
		})
		return
	}

	switch deploymentType {
	case "ECS":
		userId := deploymentInfo.Deployment.UserId
		server.mutex.Lock()
		awsProfile, ok := server.AWSProfiles[userId]
		server.mutex.Unlock()
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to find aws profile",
			})
			return
		}

		cluster := deploymentInfo.Deployer.GetCluster()
		clusterInfo, err := awsecs.GetClusterInfo(awsProfile, cluster.(*hpaws.AWSCluster))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to get ecs cluster:" + err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"error": false,
			"data":  clusterInfo,
		})
	case "K8S":
		clusterInfo, err := deploymentInfo.Deployer.(*kubernetes.K8SDeployer).GetClusterInfo()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to get kubernetes cluster:" + err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"error": false,
			"data":  clusterInfo,
		})
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": true,
			"data":  "Unsupported deployment type: " + deploymentType,
		})
	}
}
