// Package klogtolog overrides output writer for klog to log messages with log.
//
// Usage:
//
//	import (
//	  _ "github.com/flant/kube-client/klogtolog"
//	)
package klogtolog

import (
	"flag"
	"log/slog"
	"regexp"

	log "github.com/deckhouse/deckhouse/pkg/log"
	"k8s.io/klog/v2"
)

func InitAdapter(enableDebug bool, logger *log.Logger) {
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
	klog.SetOutputBySeverity("INFO", &writer{logger: logger.With("source", "klog")})
}

type writer struct {
	logger *log.Logger
}

var klogRe = regexp.MustCompile(`^.* .*  .* (.*\d+)\] (.*)\n$`)

func (w *writer) Write(msg []byte) (n int, err error) {
	groups := klogRe.FindStringSubmatch(string(msg))

	logger := w.logger.With(
		slog.String("file_and_line", groups[1]),
	)

	message := groups[2] + " (" + groups[1] + ")"

	switch msg[0] {
	case 'W':
		logger.Warn(message)
	case 'E':
		logger.Error(message)
	case 'F':
		logger.Fatal(message)
	default:
		logger.Info(message)
	}
	return 0, nil
}
