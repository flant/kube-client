package test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	log "github.com/deckhouse/deckhouse/pkg/log"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/flant/kube-client/klogtologrus"
	"github.com/flant/kube-client/klogtologrus/test/service"
)

// Test that adapter is working through default import in another package
func Test_adapter_catches_klog_WarnInfoError(t *testing.T) {
	g := NewWithT(t)

	buf := gbytes.NewBuffer()

	logger := log.NewLogger(log.Options{})
	logger.SetOutput(buf)
	klogtologrus.InitAdapter(false, logger)

	tests := []struct {
		level string
		msg   string
	}{
		{
			"warn",
			"Warning from klog powered lib",
		},
		{
			"info",
			"Info from klog powered lib",
		},
		{
			"error",
			"Error from klog powered lib",
		},
	}

	service.DoWithCallToKlogPoweredLib()

	// Catch log lines
	lines := []string{}
	scanner := bufio.NewScanner(buf)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	g.Expect(lines).To(HaveLen(len(tests)))

	for i, line := range lines {
		tt := tests[i]

		var record map[string]string
		err := json.Unmarshal([]byte(line), &record)
		g.Expect(err).ShouldNot(HaveOccurred(), line, "log line should be a valid JSON")

		g.Expect(record).Should(HaveKey("level"))
		g.Expect(record["level"]).Should(Equal(tt.level))
		g.Expect(record).Should(HaveKey("msg"))
		g.Expect(record["msg"]).Should(ContainSubstring(tt.msg))
	}
}

// Test that klog do not print to stderr
func Test_klog_should_not_output_to_Stderr(t *testing.T) {
	g := NewWithT(t)

	logger := log.NewNop()
	log.SetDefault(logger)

	stderr := captureStderr(func() {
		fmt.Fprintf(os.Stderr, "asdasdasd")
		// service.DoWithCallToKlogPoweredLib()
	})

	g.Expect(stderr).ShouldNot(ContainSubstring("klog powered lib"))
}

func captureStderr(f func()) string {
	// save and defer restore of original stderr
	origStderr := os.Stderr
	defer func() {
		os.Stderr = origStderr
	}()

	// Create a pipe to catch stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stderr = writer

	var out string
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		started := true
		go func() {
			f()
			started = false
		}()

		var buf bytes.Buffer
		b := make([]byte, 1024)
		for {
			n, err := reader.Read(b)
			if n > 0 {
				buf.Write(b[0:n])
			}
			if err != nil || n < len(b) || !started {
				break
			}
		}
		out = buf.String()

		wg.Done()
	}()
	wg.Wait()
	_ = writer.Close()
	return out
}
