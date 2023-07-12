package fake

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"

	klient "github.com/flant/kube-client/client"
	"github.com/flant/kube-client/manifest"
)

type Cluster struct {
	Client klient.Client

	Discovery *fakediscovery.FakeDiscovery
	gvrList   map[schema.GroupVersionResource]string
}

func NewFakeCluster(ver ClusterVersion) *Cluster {
	if ver == "" {
		ver = ClusterVersionV125
	}
	cres := ClusterResources(ver)

	gvrToListKind := make(map[schema.GroupVersionResource]string)
	for _, gr := range cres {
		for _, res := range gr.APIResources {
			gvr := schema.GroupVersionResource{
				Group:    res.Group,
				Version:  res.Version,
				Resource: res.Name,
			}
			gvrToListKind[gvr] = res.Kind + "List"
		}
	}

	fc := &Cluster{
		gvrList: gvrToListKind,
	}
	fc.Client = klient.NewFake(gvrToListKind)

	var ok bool
	fc.Discovery, ok = fc.Client.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		panic("couldn't convert Discovery() to *FakeDiscovery")
	}
	fc.Discovery.FakedServerVersion = &version.Info{GitCommit: ver.String(), Major: ver.Major(), Minor: ver.Minor()}
	fc.Discovery.Resources = cres

	return fc
}

func (fc *Cluster) reloadDynamicClient() {
	fc.Client.ReloadDynamic(fc.gvrList)
}

func (fc *Cluster) CreateNs(ns string) {
	nsObj := &corev1.Namespace{}
	nsObj.Name = ns
	_, _ = fc.Client.CoreV1().Namespaces().Create(context.TODO(), nsObj, metav1.CreateOptions{})
}

// RegisterCRD registers custom resources for the cluster
func (fc *Cluster) RegisterCRD(group, version, kind string, namespaced bool) {
	gvk := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
	pluralGVR, _ := meta.UnsafeGuessKindToResource(gvk)

	if _, ok := fc.gvrList[pluralGVR]; ok {
		return
	}

	fc.gvrList[pluralGVR] = kind + "List"
	fc.reloadDynamicClient()

	newResource := metav1.APIResource{
		Kind:       kind,
		Name:       pluralGVR.Resource,
		Verbs:      metav1.Verbs{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"},
		Group:      group,
		Version:    version,
		Namespaced: namespaced,
	}
	for _, resource := range fc.Discovery.Resources {
		if resource.GroupVersion == group+"/"+version {
			resource.APIResources = append(resource.APIResources, newResource)
			return
		}
	}
	fc.Discovery.Resources = append(fc.Discovery.Resources, &metav1.APIResourceList{
		GroupVersion: group + "/" + version,
		APIResources: []metav1.APIResource{newResource},
	})
}

func (fc *Cluster) FindGVR(apiVersion, kind string) (*schema.GroupVersionResource, error) {
	gvr := findGvr(fc.Discovery.Resources, apiVersion, kind)
	if gvr == nil {
		return nil, fmt.Errorf("GVR for %s is not find", kind)
	}
	return gvr, nil
}

func (fc *Cluster) MustFindGVR(apiVersion, kind string) *schema.GroupVersionResource {
	return findGvr(fc.Discovery.Resources, apiVersion, kind)
}

func (fc *Cluster) CreateSimpleNamespaced(ns, kind, name string) {
	fc.CreateNs(ns)

	gvr := fc.MustFindGVR("", kind)
	obj := manifest.New(gvr.GroupVersion().String(), kind, name).Unstructured()

	_, err := fc.Client.Dynamic().Resource(*gvr).Namespace(ns).Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}
}

func (fc *Cluster) DeleteSimpleNamespaced(ns, kind, name string) {
	gvr := fc.MustFindGVR("", kind)
	err := fc.Client.Dynamic().Resource(*gvr).Namespace(ns).Delete(context.TODO(), name, metav1.DeleteOptions{})
	if err != nil {
		panic(err)
	}
}

func (fc *Cluster) Create(ns string, m manifest.Manifest) error {
	gvr, err := fc.FindGVR(m.ApiVersion(), m.Kind())
	if err != nil {
		return err
	}
	_, err = fc.Client.Dynamic().Resource(*gvr).Namespace(m.Namespace(ns)).Create(context.TODO(), m.Unstructured(), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating object failed: %v", err)
	}
	return nil
}

func (fc *Cluster) Delete(ns string, m manifest.Manifest) error {
	gvr, err := fc.FindGVR(m.ApiVersion(), m.Kind())
	if err != nil {
		return err
	}

	err = fc.Client.Dynamic().Resource(*gvr).Namespace(m.Namespace(ns)).Delete(context.TODO(), m.Name(), metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting object failed: %v", err)
	}
	return nil
}

func (fc *Cluster) Update(ns string, m manifest.Manifest) error {
	gvr, err := fc.FindGVR(m.ApiVersion(), m.Kind())
	if err != nil {
		return err
	}

	_, err = fc.Client.Dynamic().Resource(*gvr).Namespace(m.Namespace(ns)).Update(context.TODO(), m.Unstructured(), metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating object failed: %v", err)
	}
	return nil
}

func findGvr(resources []*metav1.APIResourceList, apiVersion, kindOrName string) *schema.GroupVersionResource {
	for _, apiResourceGroup := range resources {
		if apiVersion != "" && apiResourceGroup.GroupVersion != apiVersion {
			continue
		}
		for _, apiResource := range apiResourceGroup.APIResources {
			if strings.EqualFold(apiResource.Kind, kindOrName) || strings.EqualFold(apiResource.Name, kindOrName) {
				// ignore parse error, because FakeClusterResources should be valid
				gv, _ := schema.ParseGroupVersion(apiResourceGroup.GroupVersion)
				return &schema.GroupVersionResource{
					Resource: apiResource.Name,
					Group:    gv.Group,
					Version:  gv.Version,
				}
			}
		}
	}
	return nil
}
