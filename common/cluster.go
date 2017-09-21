package common

import (
	"errors"
	"fmt"

	"github.com/hyperpilotio/deployer/apis"
	hpaws "github.com/hyperpilotio/deployer/clusters/aws"
	logging "github.com/op/go-logging"
)

func UploadFiles(
	awsCluster *hpaws.AWSCluster,
	deployment *apis.Deployment,
	uploadedFiles map[string]string,
	bastionIp string, log *logging.Logger) error {
	if len(deployment.Files) == 0 {
		return nil
	}

	clientConfig, clientConfigErr := awsCluster.SshConfig("ubuntu")
	if clientConfigErr != nil {
		return errors.New("Unable to create ssh config: " + clientConfigErr.Error())
	}

	for _, nodeInfo := range awsCluster.NodeInfos {
		sshClient := NewSshClient(nodeInfo.PrivateIp+":22", clientConfig, bastionIp+":22")
		// TODO: Refactor this so can be reused with AWS
		for _, deployFile := range deployment.Files {
			// TODO: Bulk upload all files, where ssh client needs to support multiple files transfer
			// in the same connection
			location, ok := uploadedFiles[deployment.UserId+"_"+deployFile.FileId]
			if !ok {
				return errors.New("Unable to find uploaded file " + deployFile.FileId)
			}

			if err := sshClient.CopyLocalFileToRemote(location, deployFile.Path); err != nil {
				return fmt.Errorf("Unable to upload file %s to server %s: %s",
					deployFile.FileId, nodeInfo.PrivateIp, err.Error())
			}
		}
	}

	log.Info("Uploaded all files")
	return nil
}
