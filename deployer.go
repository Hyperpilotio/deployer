package main

import (
	"os"

	cli "github.com/urfave/cli"
)

func Run(cluster string, port string) error {
	return StartServer(port)
}

func main() {
	var cluster = ""
	// Parse parameters from command line input.
	app := cli.NewApp()
	app.Name = "deployer"
	app.Usage = "Pilot for deploying environments and workloads"
	// Global flags
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:        "cluster",
			Usage:       "The name of target cluster",
			Destination: &cluster,
		},
		cli.StringFlag{
			Name:  "port",
			Value: "7777",
			Usage: "The port of scheduler REST server",
		},
	}
	app.Action = func(c *cli.Context) error {
		return Run(c.String("cluster"), c.String("port"))
	}

	app.Run(os.Args)
}
