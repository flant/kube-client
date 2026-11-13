package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"
)

// noWatchListClientset mimics client-go 0.35+ fake.Clientset, which opts out of WatchList.
type noWatchListClientset struct{ kubernetes.Interface }

func (noWatchListClientset) IsWatchListSemanticsUnSupported() bool { return true }

func Test_IsWatchListSemanticsUnSupported(t *testing.T) {
	assert.True(t, (&Client{Interface: noWatchListClientset{}}).IsWatchListSemanticsUnSupported(), "fake opt-out must reach client-go")
	assert.False(t, (&Client{Interface: &kubernetes.Clientset{}}).IsWatchListSemanticsUnSupported(), "real clientset must keep WatchList")
}
