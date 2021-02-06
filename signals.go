package main

import (
	"os"
	"syscall"

	"github.com/sirupsen/logrus"
)

var handledSignals = []os.Signal{
	syscall.SIGTERM,
	syscall.SIGINT,
}

func handleSignals(signals chan os.Signal) chan struct{} {
	done := make(chan struct{}, 1)
	go func() {
		s := <-signals
		logrus.Infof("received a signal %s", s)
		done <- struct{}{}
		close(done)
	}()
	return done
}
