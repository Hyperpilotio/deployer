package main

import (
	"os"

	"github.com/spf13/viper"
	cli "github.com/urfave/cli"
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
	return server.StartServer()
}

func main() {
	var fileConfig string
	// Parse parameters from command line input.
	app := cli.NewApp()
	app.Name = "deployer"
	app.Usage = "Pilot for deploying environments and workloads"
	// Global flags
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:        "config",
			Usage:       "The file path to a config file",
			Destination: &fileConfig,
		},
	}
	app.Action = func(c *cli.Context) error {
		return Run(fileConfig)
	}

	app.Run(os.Args)
}
