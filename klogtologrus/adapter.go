// Package klogtologrus overrides output writer for klog to log messages with logrus.
//
// Usage:
//
//   import (
//     _ "github.com/flant/kube-client/klogtologrus"
//   )
package klogtologrus

import (
	"flag"

	log "github.com/sirupsen/logrus"
	"k8s.io/klog/v2"
)

func InitAdapter(enableDebug bool) {
	// - turn off logging to stderr
	// - default stderr threshold is ERROR and it outputs errors to stderr, set it to FATAL
	// - set writer for INFO severity to catch all messages
	klogFlagSet := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlagSet)
	args := []string{
		"-logtostderr=false",
		"-stderrthreshold=FATAL",
	}

	if enableDebug {
		args = append(args, "-v=10")
	}

	_ = klogFlagSet.Parse(args)
	klog.SetOutputBySeverity("INFO", &writer{logger: log.WithField("source", "klog")})
}

type writer struct {
	logger *log.Entry
}

func (w *writer) Write(msg []byte) (n int, err error) {
	switch msg[0] {
	case 'W':
		w.logger.Warn(string(msg))
	case 'E':
		w.logger.Error(string(msg))
	case 'F':
		w.logger.Fatal(string(msg))
	default:
		w.logger.Info(string(msg))
	}
	return 0, nil
}
