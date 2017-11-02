package common

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/spf13/viper"
)

type Downloader interface {
	Download(fileUrl string) (string, error)
}

type S3Downloader struct {
	config    *viper.Viper
	region    string
	awsId     string
	awsSecret string
}

func NewS3Downloader(config *viper.Viper) (Downloader, error) {
	return &S3Downloader{
		config:    config,
		region:    config.GetString("s3.region"),
		awsId:     config.GetString("awsId"),
		awsSecret: config.GetString("awsSecret"),
	}, nil
}

func (files *S3Downloader) Download(s3FileUrl string) (string, error) {
	if !strings.HasPrefix(s3FileUrl, "s3://") {
		return "", errors.New("Unsupported file url")
	}

	urls := strings.Split(strings.Replace(s3FileUrl, "s3://", "", 1), "/")
	bucket := urls[0]
	key := urls[1]

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(files.region),
		Credentials: credentials.NewStaticCredentials(files.awsId, files.awsSecret, ""),
	})
	if err != nil {
		return "", errors.New("Unable to create aws session: " + err.Error())
	}

	downloader := s3manager.NewDownloader(sess)
	tmpFile, err := ioutil.TempFile("", key+".tmp")
	if err != nil {
		return "", errors.New("Unable to create temp file: " + err.Error())
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()

	_, err = downloader.Download(tmpFile,
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
	if err != nil {
		return "", fmt.Errorf("Unable to download %s from bucket %s: %v", key, bucket, err)
	}

	destination := path.Join(files.config.GetString("filesPath"), key)
	if err := os.Rename(tmpFile.Name(), destination); err != nil {
		return "", errors.New("Unable to rename file: " + err.Error())
	}

	return destination, nil
}
