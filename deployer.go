package main

import (
	"flag"

	"github.com/golang/glog"

	"github.com/spf13/viper"
)

// Run start the web server
func Run(fileConfig string) error {
	viper := viper.New()
	viper.SetConfigType("json")

	if fileConfig == "" {
		viper.SetConfigName("config")
		viper.AddConfigPath("/etc/deployer")
	} else {
		viper.SetConfigFile(fileConfig)
	}

	err := viper.ReadInConfig()
	if err != nil {
		return err
	}

	server := NewServer(viper)
	server.loadDeploymentStatus()
	return server.StartServer()
}

func main() {
	configPath := flag.String("config", "", "The file path to a config file")
	flag.Parse()

	err := Run(*configPath)
	glog.Errorln(err)
}
