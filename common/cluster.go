package common

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"

	"github.com/hyperpilotio/deployer/apis"
	logging "github.com/op/go-logging"
)

func UploadFiles(
	config *viper.Viper,
	sshClient SshClient,
	deployment *apis.Deployment,
	uploadedFiles map[string]string,
	log *logging.Logger) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	var s3Downloader Downloader
	for _, deployFile := range deployment.Files {
		if deployFile.FileUrl != "" {
			downloader, err := NewS3Downloader(config)
			if err != nil {
				return errors.New("Uable to init S3Downloader: " + err.Error())
			}
			s3Downloader = downloader
			break
		}
	}

	for _, deployFile := range deployment.Files {
		uploadFilePath := ""
		if deployFile.FileUrl != "" {
			location, err := s3Downloader.Download(deployFile.FileUrl)
			if err != nil {
				return errors.New("Uable to download file: " + err.Error())
			}
			uploadFilePath = location
		} else {
			location, ok := uploadedFiles[deployment.UserId+"_"+deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}
			uploadFilePath = location
		}

		if err := sshClient.CopyLocalFileToRemote(uploadFilePath, deployFile.Path); err != nil {
			return fmt.Errorf("Unable to upload file %s to server %s:%s: %s",
				deployFile.FileId, sshClient.Host, deployFile.Path, err.Error())
		}
	}

	log.Info("Uploaded files to " + sshClient.Host)
	return nil
}
