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

// examples
//
// from this
//
// I0704 19:13:35.888843  135000 lib_methods.go:12] Info from klog powered lib\n
// W0704 19:13:35.888120  135000 lib_methods.go:8] Warning from klog powered lib\n
// E0704 19:13:35.888852  135000 adapter_test.go:48] Error from klog powered lib\n
//
// to this
//
// {"level":"warn","msg":"Warning from klog powered lib (lib_methods.go:8)","file_and_line":"lib_methods.go:8","time":"2025-07-04T19:39:28+03:00"}
// {"level":"info","msg":"Info from klog powered lib (lib_methods.go:12)","file_and_line":"lib_methods.go:12","time":"2025-07-04T19:39:28+03:00"}
// {"level":"error","msg":"Error from klog powered lib (adapter_test.go:48)","file_and_line":"adapter_test.go:48", "stacktrace": ... ,"time":"2025-07-04T19:39:28+03:00"}
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
