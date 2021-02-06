package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func init() {
	logrus.SetLevel(logrus.DebugLevel)
}

func main() {
	app := cli.NewApp()
	app.Name = "startup-exporter"
	app.Usage = "A tool to collect and export container startup time"
	app.Commands = []cli.Command{
		collectCmd,
		exportCmd,
	}
	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
