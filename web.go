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
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	hpgcp "github.com/hyperpilotio/deployer/clusters/gcp"
)

type UserProfileData interface {
	Store() error
	Load() (interface{}, error)
	Delete() error
}

type AWSUserProfileData struct {
	AWSProfile *hpaws.AWSProfile
	Server     *Server
}

type GCPUserProfileData struct {
	GCPProfile *hpgcp.GCPProfile
	FileId     string
	Server     *Server
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

func (server *Server) NewUserProfileData(c *gin.Context) (UserProfileData, error) {
	clusterType := c.DefaultQuery("clusterType", "")
	userId := c.DefaultQuery("userId", "")
	switch clusterType {
	case "AWS":
		return &AWSUserProfileData{
			AWSProfile: &hpaws.AWSProfile{
				UserId:    userId,
				AwsId:     c.DefaultQuery("awsId", ""),
				AwsSecret: c.DefaultQuery("awsSecret", ""),
			},
			Server: server,
		}, nil
	case "GCP":
		gcpProfile := &hpgcp.GCPProfile{
			UserId:           userId,
			ServiceAccount:   c.DefaultQuery("serviceAccount", ""),
			AuthJSONFilePath: c.DefaultQuery("filePath", ""),
		}

		return &GCPUserProfileData{
			FileId:     c.DefaultQuery("fileId", ""),
			GCPProfile: gcpProfile,
			Server:     server,
		}, nil
	default:
		return nil, errors.New("Unsupported cluster type: " + clusterType)
	}
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
	userProfileData, err := server.NewUserProfileData(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to new user profile data: " + err.Error(),
		})
		return
	}

	if err := userProfileData.Store(); err != nil {
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

func (server *Server) getUser(c *gin.Context) {
	userProfileData, err := server.NewUserProfileData(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to new user profile data: " + err.Error(),
		})
		return
	}

	userProfile, err := userProfileData.Load()
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": true,
			"data":  "Unable to find user data:" + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  userProfile,
	})
}

func (server *Server) deleteUser(c *gin.Context) {
	userProfileData, err := server.NewUserProfileData(c)
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
			"data":  "UUnable to delete user data:" + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"error": false,
		"data":  "",
	})
}

func (awsUser *AWSUserProfileData) Store() error {
	awsProfile := awsUser.AWSProfile
	userId := awsUser.AWSProfile.UserId
	if awsProfile.AwsId == "" {
		return errors.New("Unable to find awsId param")
	}
	if awsProfile.AwsSecret == "" {
		return errors.New("Unable to find awsSecret param")
	}

	if err := awsUser.Server.AWSProfileStore.Store(userId, awsProfile); err != nil {
		return errors.New("Unable to store user data: " + err.Error())
	}

	awsUser.Server.mutex.Lock()
	awsUser.Server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
		AWSProfile: awsProfile,
	}
	awsUser.Server.mutex.Unlock()

	return nil
}

func (awsUser *AWSUserProfileData) Load() (interface{}, error) {
	userId := awsUser.AWSProfile.UserId
	userProfile, ok := awsUser.Server.DeploymentUserProfiles[userId]
	if !ok {
		return nil, errors.New("Unable to find AWS user profile")
	}

	return userProfile.GetAWSProfile(), nil
}

func (awsUser *AWSUserProfileData) Delete() error {
	userId := awsUser.AWSProfile.UserId
	if err := awsUser.Server.AWSProfileStore.Delete(userId); err != nil {
		return errors.New("Unable to delete user data: " + err.Error())
	}

	userProfile, ok := awsUser.Server.DeploymentUserProfiles[userId]
	if ok {
		awsUser.Server.mutex.Lock()
		if userProfile.GetGCPProfile() != nil {
			awsUser.Server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				GCPProfile: userProfile.GetGCPProfile(),
			}
		} else {
			delete(awsUser.Server.DeploymentUserProfiles, userId)
		}
		awsUser.Server.mutex.Unlock()
	}

	return nil
}

func (gcpUser *GCPUserProfileData) Store() error {
	gcpProfile := gcpUser.GCPProfile
	if gcpProfile.ServiceAccount == "" {
		return errors.New("Unable to find serviceAccount param")
	}

	userId := gcpProfile.UserId
	if gcpProfile.AuthJSONFilePath == "" {
		fileId := gcpUser.FileId
		fileName := userId + "_" + fileId
		filePath, ok := gcpUser.Server.UploadedFiles[fileName]
		if !ok {
			return errors.New("Unable to find uploaded file " + fileId)
		}

		if err := hpgcp.UploadFilesToStorage(gcpUser.Server.Config, fileName, filePath); err != nil {
			return errors.New("Unable to upload file to GCP strorage: " + err.Error())
		}
		gcpProfile.AuthJSONFilePath = filePath

		projectId, err := gcpProfile.GetProjectId()
		if err != nil {
			return errors.New("Unable to find projectId: " + err.Error())
		}
		gcpProfile.ProjectId = projectId
	}

	if err := gcpUser.Server.GCPProfileStore.Store(userId, gcpProfile); err != nil {
		return errors.New("Unable to store user data: " + err.Error())
	}

	gcpUser.Server.mutex.Lock()
	gcpUser.Server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
		GCPProfile: gcpProfile,
	}
	gcpUser.Server.mutex.Unlock()

	return nil
}

func (gcpUser *GCPUserProfileData) Load() (interface{}, error) {
	userId := gcpUser.GCPProfile.UserId
	userProfile, ok := gcpUser.Server.DeploymentUserProfiles[userId]
	if !ok {
		return nil, errors.New("Unable to find GCP user profile")
	}

	return userProfile.GetGCPProfile(), nil
}

func (gcpUser *GCPUserProfileData) Delete() error {
	userId := gcpUser.GCPProfile.UserId
	if err := gcpUser.Server.GCPProfileStore.Delete(userId); err != nil {
		return errors.New("Unable to delete user data: " + err.Error())
	}

	// TODO delete GCP storage service account

	userProfile, ok := gcpUser.Server.DeploymentUserProfiles[userId]
	if ok {
		gcpUser.Server.mutex.Lock()
		if userProfile.GetAWSProfile() != nil {
			gcpUser.Server.DeploymentUserProfiles[userId] = &DeploymentUserProfile{
				AWSProfile: userProfile.GetAWSProfile(),
			}
		} else {
			delete(gcpUser.Server.DeploymentUserProfiles, userId)
		}
		gcpUser.Server.mutex.Unlock()
	}

	return nil
}
