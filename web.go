package main

import (
	"bufio"
	"errors"
	"os"
	"path"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperpilotio/deployer/awsecs"

	"net/http"
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

func (server *Server) logUI(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentLogs := server.getDeploymentLogs()
	c.HTML(http.StatusOK, "index.html", gin.H{
		"msg":  "Hyperpilot Deployments!",
		"logs": deploymentLogs,
	})
}

func (server *Server) userUI(c *gin.Context) {
	awsProfiles, err := server.Store.LoadAWSProfiles()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": true,
			"data":  "Unable to load aws profile: " + err.Error(),
		})
		return
	}

	c.HTML(http.StatusOK, "user.html", gin.H{
		"error":       false,
		"awsProfiles": awsProfiles,
	})
}

func (server *Server) clusterUI(c *gin.Context) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	deploymentLogs := server.getDeploymentLogs()
	c.HTML(http.StatusOK, "cluster.html", gin.H{
		"msg":  "Hyperpilot Clusters!",
		"logs": deploymentLogs,
	})
}

func (server *Server) getDeploymentLog(c *gin.Context) {
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

func (server *Server) getDeploymentLogs() []*DeploymentLog {
	deploymentLogs := DeploymentLogs{}
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

func (server *Server) storeUser(c *gin.Context) {
	awsProfile, err := getAWSProfileParam(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to get aws profile param: " + err.Error(),
		})
		return
	}

	if err := server.Store.StoreNewAWSProfile(awsProfile); err != nil {
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

	if err := server.Store.DeleteAWSProfile(userId); err != nil {
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

func getAWSProfileParam(c *gin.Context) (*awsecs.AWSProfile, error) {
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

	awsProfile := &awsecs.AWSProfile{
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

	switch deploymentType {
	case "ECS":
		server.mutex.Lock()
		deploymentInfo, ok := server.DeployedClusters[clusterName]
		if !ok {
			server.mutex.Unlock()
			c.JSON(http.StatusNotFound, gin.H{
				"error": true,
				"data":  "Deployment not found",
			})
			return
		}
		server.mutex.Unlock()

		deployments, err := server.Store.LoadDeployments()
		if err != nil {
			server.mutex.Unlock()
			c.JSON(http.StatusNotFound, gin.H{
				"error": true,
				"data":  "Unable to load deployment status:" + err.Error(),
			})
			return
		}

		userId := ""
		for _, deployment := range deployments {
			if deployment.Name == clusterName {
				userId = deployment.UserId
				break
			}
		}

		server.mutex.Lock()
		awsProfile, ok := server.AWSProfiles[userId]
		if !ok {
			server.mutex.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{
				"error": true,
				"data":  "Unable to find aws profile",
			})
			return
		}
		server.mutex.Unlock()

		clusterInfo, err := awsecs.GetClusterInfo(awsProfile, deploymentInfo.awsInfo)
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
		server.mutex.Lock()
		k8sDeployment, ok := server.KubernetesClusters.Clusters[clusterName]
		if !ok {
			server.mutex.Unlock()
			c.JSON(http.StatusNotFound, gin.H{
				"error": true,
				"data":  "Deployment not found",
			})
			return
		}
		server.mutex.Unlock()

		clusterInfo, err := k8sDeployment.GetClusterInfo()
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
			"data":  "Unsupported deployment type",
		})
	}
}
