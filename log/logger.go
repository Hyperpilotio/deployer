package log

import (
	"errors"
	"os"
	"path"

	logging "github.com/op/go-logging"
	"github.com/spf13/viper"
)

var logFormatter = logging.MustStringFormatter(
	` %{level:.1s}%{time:0102 15:04:05.999999} %{pid} %{shortfile}] %{message}`,
)

type DeploymentLog struct {
	Name    string
	Logger  *logging.Logger
	LogFile *os.File
}

// NewLogger create per deployment logger
func NewLogger(config *viper.Viper, deploymentName string) (*DeploymentLog, error) {
	log := logging.MustGetLogger(deploymentName)

	logDirPath := path.Join(config.GetString("filesPath"), "log")
	if _, err := os.Stat(logDirPath); os.IsNotExist(err) {
		os.Mkdir(logDirPath, 0777)
	}

	logFilePath := path.Join(logDirPath, deploymentName+".log")
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, errors.New("Unable to create deployment log file:" + err.Error())
	}

	fileLog := logging.NewLogBackend(logFile, "["+deploymentName+"]", 0)
	consoleLog := logging.NewLogBackend(os.Stdout, "["+deploymentName+"]", 0)

	fileLogLevel := logging.AddModuleLevel(fileLog)
	fileLogLevel.SetLevel(logging.INFO, "")

	consoleLogBackend := logging.NewBackendFormatter(consoleLog, logFormatter)
	fileLogBackend := logging.NewBackendFormatter(fileLog, logFormatter)

	log.SetBackend(logging.SetBackend(fileLogBackend, consoleLogBackend))

	return &DeploymentLog{
		Name:    deploymentName,
		Logger:  log,
		LogFile: logFile,
	}, nil
}
