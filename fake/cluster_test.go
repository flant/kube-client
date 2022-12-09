package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRegisterCRD(t *testing.T) {
	f := NewFakeCluster("")

	t.Run("test CRD registration", func(t *testing.T) {
		f.RegisterCRD("deckhouse.io", "v1alpha1", "KeepalivedInstance", false)
		gvk := schema.GroupVersionResource{
			Group:    "deckhouse.io",
			Version:  "v1alpha1",
			Resource: "keepalivedinstances",
		}
		_, err := f.Client.Dynamic().Resource(gvk).Namespace("").List(context.TODO(), v1.ListOptions{})
		require.NoError(t, err)

		_, err = f.Client.Dynamic().Resource(gvk).Namespace("").Get(context.TODO(), "foo", v1.GetOptions{})
		require.ErrorContains(t, err, "not found")

		// register next CRD
		f.RegisterCRD("deckhouse.io", "v1", "NodeGroup", false)

		// new crd exists
		ngGVR := schema.GroupVersionResource{
			Group:    "deckhouse.io",
			Version:  "v1",
			Resource: "nodegroups",
		}
		_, err = f.Client.Dynamic().Resource(ngGVR).Namespace("").List(context.TODO(), v1.ListOptions{})
		require.NoError(t, err)

		// previous crd exists
		_, err = f.Client.Dynamic().Resource(gvk).Namespace("").List(context.TODO(), v1.ListOptions{})
		require.NoError(t, err)
	})

	t.Run("test default resources", func(t *testing.T) {
		_, err := f.Client.Dynamic().Resource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "pods",
		}).Namespace("").List(context.TODO(), v1.ListOptions{})
		require.NoError(t, err)
	})
}
