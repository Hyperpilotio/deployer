package main

import (
	"bufio"
	"errors"
	"net/http"
	"os"
	"path"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/glog"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
)

type UserProfileData interface {
	Store() error
	Delete() error
}

type ClusterUserProfileData struct {
	DeploymentUserProfile
	server *Server
}

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

func (server *Server) NewClusterUserProfileData(c *gin.Context) (UserProfileData, error) {
	userId := c.DefaultPostForm("userId", "")
	if userId == "" {
		userId = c.Param("userId")
	}

	return &ClusterUserProfileData{
		DeploymentUserProfile: DeploymentUserProfile{
			UserId: userId,
			AWSProfile: &hpaws.AWSProfile{
				UserId:    userId,
				AwsId:     c.DefaultPostForm("awsId", ""),
				AwsSecret: c.DefaultPostForm("awsSecret", ""),
			},
			GCPProfile: &hpgcp.GCPProfile{
				UserId:              userId,
				ServiceAccount:      c.DefaultPostForm("serviceAccount", ""),
				AuthJSONFileContent: c.DefaultPostForm("authJSONFileContent", ""),
			},
		},
		server: server,
	}, nil
}

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
		"error":        false,
		"userProfiles": server.DeploymentUserProfiles,
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
	for userId, _ := range server.DeploymentUserProfiles {
		userIds = append(userIds, userId)
	}
	return userIds
}

func (server *Server) storeUser(c *gin.Context) {
	userProfileData, err := server.NewClusterUserProfileData(c)
	if err != nil {
		glog.Warning("Unable to create user profile data: " + err.Error())
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to new user profile data: " + err.Error(),
		})
		return
	}

	if err := userProfileData.Store(); err != nil {
		glog.Warning("Unable to store profile data: " + err.Error())
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to store user data: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
	})
}

func (server *Server) deleteUser(c *gin.Context) {
	userProfileData, err := server.NewClusterUserProfileData(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to new user profile data: " + err.Error(),
		})
		return
	}

	if err := userProfileData.Delete(); err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to delete user data:" + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
	})
}

func (profileData *ClusterUserProfileData) Store() error {
	server := profileData.server
	userId := profileData.UserId
	deploymentUserProfile := profileData.DeploymentUserProfile
	if err := server.ProfileStore.Store(userId, &deploymentUserProfile); err != nil {
		return errors.New("Unable to store user data: " + err.Error())
	}

	server.mutex.Lock()
	server.DeploymentUserProfiles[userId] = &deploymentUserProfile
	server.mutex.Unlock()

	return nil
}

func (profileData *ClusterUserProfileData) Delete() error {
	server := profileData.server
	userId := profileData.UserId
	if err := server.ProfileStore.Delete(userId); err != nil {
		return errors.New("Unable to delete user data: " + err.Error())
	}

	server.mutex.Lock()
	delete(server.DeploymentUserProfiles, userId)
	server.mutex.Unlock()

	return nil
}
