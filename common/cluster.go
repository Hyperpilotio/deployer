package common

import (
	"errors"
	"fmt"

	"github.com/hyperpilotio/deployer/apis"
	logging "github.com/op/go-logging"
)

func UploadFiles(
	sshClient SshClient,
	deployment *apis.Deployment,
	uploadedFiles map[string]string,
	log *logging.Logger) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	for _, deployFile := range deployment.Files {
		location, ok := uploadedFiles[deployment.UserId+"_"+deployFile.FileId]
		if !ok {
			return errors.New("Unable to find uploaded file " + deployFile.FileId)
		}

		if err := sshClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
			return fmt.Errorf("Unable to upload file %s to server %s: %s",
				deployFile.FileId, sshClient.Host, err.Error())
		}
	}

	log.Info("Uploaded files to " + sshClient.Host)
	return nil
}
