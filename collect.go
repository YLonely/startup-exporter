package main

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	defaultContainerdRoot = "/run/containerd/io.containerd.runtime.v2.task"
	waitPeriod            = 1 * time.Second
)

var collectCmd = cli.Command{
	Name:      "collect",
	Usage:     "collect startup time of containers from containerd",
	ArgsUsage: "EXPORTER_IP:PORT",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "namespace,n",
			Usage: "specifiy the namespace of containers should be collected",
		},
	},
	Action: func(context *cli.Context) error {
		addr := context.Args().First()
		if addr == "" {
			return errors.New("address of exporter must be provided")
		}
		signalC := make(chan os.Signal, 1024)
		signal.Notify(signalC, handledSignals...)
		done := handleSignals(signalC)
		err := os.Chdir(defaultContainerdRoot)
		if err != nil {
			return errors.Wrap(err, "failed to change the work dir")
		}
		ns := context.String("namespace")
		ticker := time.NewTicker(waitPeriod)
		exit := false
		for {
			all := []containerStartupInfo{}
			if ns == "" {
				dirs, err := ioutil.ReadDir(".")
				if err != nil {
					return errors.Wrap(err, "failed to read the current dir")
				}
				for _, dir := range dirs {
					all = append(all, collect(dir.Name())...)
				}
			} else {
				all = append(all, collect(ns)...)
			}
			if err := push(all, addr); err != nil {
				logrus.WithError(err).Error("failed to push container startup info to the exporter")
			}
			select {
			case <-ticker.C:
			case <-done:
				exit = true
			}
			if exit {
				break
			}
		}
		return nil
	},
}

func push(info []containerStartupInfo, addr string) error {
	for _, i := range info {
		bs, err := json.Marshal(i)
		if err != nil {
			return err
		}
		resp, err := http.Post(addr, "application/json", bytes.NewReader(bs))
		if err != nil {
			return errors.Wrap(err, "failed to post the info")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("received status %s from server", resp.Status)
		}
	}
	return nil
}

func collect(namespace string) []containerStartupInfo {
	var info []containerStartupInfo
	dirs, err := ioutil.ReadDir(namespace)
	if err != nil {
		logrus.WithError(err).Error()
		return info
	}
	for _, dir := range dirs {
		startupPath := path.Join(namespace, dir.Name(), "startup")
		if _, err := os.Stat(startupPath); err != nil {
			continue
		}
		bs, err := ioutil.ReadFile(startupPath)
		if err != nil {
			logrus.WithError(err).Errorf("failed to read content from %s", startupPath)
			continue
		}
		content := strings.Trim(string(bs), " \t\n")
		lines := strings.Split(content, "\n")
		n := len(lines)
		if n < 2 {
			continue
		}
		start, err := strconv.Atoi(lines[0])
		if err != nil {
			logrus.WithError(err).Errorf("invalid start time %q from container %s", lines[0], dir.Name())
			continue
		}
		end, err := strconv.Atoi(lines[1])
		if err != nil {
			logrus.WithError(err).Errorf("invalid end time %q from container %s", lines[1], dir.Name())
			continue
		}
		if end == 0 {
			continue
		}
		info = append(info, containerStartupInfo{
			Name:      dir.Name(),
			Namespace: namespace,
			Start:     int64(start),
			End:       int64(end),
		})
	}
	return info
}
