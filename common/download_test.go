package common

import (
	"log"
	"testing"

	"github.com/spf13/viper"
)

var config *viper.Viper

const (
	testFileUrl = "s3://hyperpilot-deployment-files/tech-demo-locustfile.py"
)

func init() {
	config = viper.New()
	config.SetConfigType("json")
	config.SetConfigFile("./../documents/deployed.config")
	config.ReadInConfig()
}

func TestDownloadFileFromS3(t *testing.T) {
	s3Downloader, err := NewS3Downloader(config)
	if err != nil {
		log.Fatalf("Unable to init S3Downloader: " + err.Error())
	}

	filePath, err := s3Downloader.Download(testFileUrl)
	if err != nil {
		log.Fatalf("Unable to download file: " + err.Error())
	}
	log.Printf("Download %s to %s", testFileUrl, filePath)
}
