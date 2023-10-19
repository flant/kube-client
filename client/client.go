package client

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/disk"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/metadata"
	fakemetadata "k8s.io/client-go/metadata/fake"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // load the gcp plugin (only required to authenticate against GKE clusters)
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/metrics"

	_ "github.com/flant/kube-client/klogtologrus" // route klog messages from client-go to logrus
)

const (
	kubeTokenFilePath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	kubeNamespaceFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

func New() *Client {
	return &Client{}
}

func NewFake(gvr map[schema.GroupVersionResource]string) *Client {
	sc := runtime.NewScheme()
	return &Client{
		Interface:        fake.NewSimpleClientset(),
		defaultNamespace: "default",
		dynamicClient:    fakedynamic.NewSimpleDynamicClientWithCustomListKinds(sc, gvr),
		metadataClient:   fakemetadata.NewSimpleMetadataClient(sc),
		schema:           sc,
	}
}

type Client struct {
	kubernetes.Interface
	cachedDiscovery  discovery.CachedDiscoveryInterface
	contextName      string
	configPath       string
	defaultNamespace string
	dynamicClient    dynamic.Interface
	apiExtClient     apixv1client.ApiextensionsV1Interface
	metadataClient   metadata.Interface
	qps              float32
	burst            int
	timeout          time.Duration
	server           string
	metricStorage    MetricStorage
	metricLabels     map[string]string
	schema           *runtime.Scheme
	restConfig       *rest.Config
}

// ReloadDynamic creates new dynamic client with the new set of CRDs.
func (c *Client) ReloadDynamic(gvrList map[schema.GroupVersionResource]string) {
	c.dynamicClient = fakedynamic.NewSimpleDynamicClientWithCustomListKinds(c.schema, gvrList)
}

func (c *Client) WithServer(server string) {
	c.server = server
}

func (c *Client) WithContextName(name string) {
	c.contextName = name
}

func (c *Client) WithConfigPath(path string) {
	c.configPath = path
}

func (c *Client) WithRateLimiterSettings(qps float32, burst int) {
	c.qps = qps
	c.burst = burst
}

func (c *Client) WithTimeout(timeout time.Duration) {
	c.timeout = timeout
}

func (c *Client) WithMetricStorage(metricStorage MetricStorage) {
	c.metricStorage = metricStorage
}

func (c *Client) WithMetricLabels(labels map[string]string) {
	c.metricLabels = labels
}

func (c *Client) DefaultNamespace() string {
	return c.defaultNamespace
}

func (c *Client) Dynamic() dynamic.Interface {
	return c.dynamicClient
}

func (c *Client) ApiExt() apixv1client.ApiextensionsV1Interface {
	return c.apiExtClient
}

func (c *Client) Metadata() metadata.Interface {
	return c.metadataClient
}

// RestConfig returns kubernetes Config with the common attributes that was passed on initialization.
func (c *Client) RestConfig() *rest.Config {
	return c.restConfig
}

func (c *Client) Init() error {
	logEntry := log.WithField("operator.component", "KubernetesAPIClient")

	var err error
	var config *rest.Config
	configType := "out-of-cluster"
	var defaultNs string

	if c.server == "" {
		// Try to load from kubeconfig in flags or from ~/.kube/config
		var outOfClusterErr error
		config, defaultNs, outOfClusterErr = getOutOfClusterConfig(c.contextName, c.configPath)

		if config == nil {
			if hasInClusterConfig() {
				// Try to configure as inCluster
				config, defaultNs, err = getInClusterConfig()
				if err != nil {
					if c.configPath != "" || c.contextName != "" {
						if outOfClusterErr != nil {
							err = fmt.Errorf("out-of-cluster config error: %v, in-cluster config error: %v", outOfClusterErr, err)
							logEntry.Errorf("configuration problems: %s", err)
							return err
						}
						return fmt.Errorf("in-cluster config is not found")
					}
					logEntry.Errorf("in-cluster problem: %s", err)
					return err
				}
			} else {
				// if not in cluster return outOfCluster error
				if outOfClusterErr != nil {
					logEntry.Errorf("out-of-cluster problem: %s", outOfClusterErr)
					return outOfClusterErr
				}
				return fmt.Errorf("no kubernetes client config found")
			}
			configType = "in-cluster"
		}
	} else {
		// use specific server to connect to API
		config = &rest.Config{
			Host: c.server,
		}
		_ = rest.SetKubernetesDefaults(config)
		defaultNs = "default"
		configType = "server"
	}

	c.defaultNamespace = defaultNs

	config.QPS = c.qps
	config.Burst = c.burst

	config.Timeout = c.timeout

	c.Interface, err = kubernetes.NewForConfig(config)
	if err != nil {
		logEntry.Errorf("configuration problem: %s", err)
		return err
	}

	c.dynamicClient, err = dynamic.NewForConfig(config)
	if err != nil {
		return err
	}

	c.apiExtClient, err = apixv1client.NewForConfig(config)
	if err != nil {
		return err
	}

	c.metadataClient, err = metadata.NewForConfig(config)
	if err != nil {
		return err
	}

	if c.metricStorage != nil {
		metrics.Register(
			metrics.RegisterOpts{
				RequestLatency: NewRateLimiterLatencyMetric(c.metricStorage),
				RequestResult:  NewRequestResultMetric(c.metricStorage, c.metricLabels),
			},
		)
	}

	cacheDiscoveryDir, err := os.MkdirTemp("", "kube-cache-discovery-*")
	if err != nil {
		return err
	}

	cacheHttpDir, err := os.MkdirTemp("", "kube-cache-http-*")
	if err != nil {
		return err
	}

	c.cachedDiscovery, err = disk.NewCachedDiscoveryClientForConfig(config, cacheDiscoveryDir, cacheHttpDir, 10*time.Minute)
	if err != nil {
		return err
	}

	c.restConfig = config
	logEntry.Infof("Kubernetes client is configured successfully with '%s' config", configType)

	return nil
}

func makeOutOfClusterClientConfigError(kubeConfig, kubeContext string, err error) error {
	baseErrMsg := "out-of-cluster configuration problem"

	if kubeConfig != "" {
		baseErrMsg += fmt.Sprintf(", custom kube config path is '%s'", kubeConfig)
	}

	if kubeContext != "" {
		baseErrMsg += fmt.Sprintf(", custom kube context is '%s'", kubeContext)
	}

	return fmt.Errorf("%s: %s", baseErrMsg, err)
}

func getClientConfig(context, kubeconfig string) clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.DefaultClientConfig = &clientcmd.DefaultClientConfig

	overrides := &clientcmd.ConfigOverrides{ClusterDefaults: clientcmd.ClusterDefaults}

	if context != "" {
		overrides.CurrentContext = context
	}

	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

func hasInClusterConfig() bool {
	token, _ := fileExists(kubeTokenFilePath)
	ns, _ := fileExists(kubeNamespaceFilePath)
	return token && ns
}

// fileExists returns true if path exists
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func getOutOfClusterConfig(contextName, configPath string) (config *rest.Config, defaultNs string, err error) {
	clientConfig := getClientConfig(contextName, configPath)

	defaultNs, _, err = clientConfig.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("cannot determine default kubernetes namespace: %s", err)
	}

	config, err = clientConfig.ClientConfig()
	if err != nil {
		return nil, "", makeOutOfClusterClientConfigError(configPath, contextName, err)
	}

	// rc, err := clientConfig.RawConfig()
	// if err != nil {
	//	return nil, fmt.Errorf("cannot get raw kubernetes config: %s", err)
	// }
	//
	// if contextName != "" {
	//	Context = contextName
	// } else {
	//	Context = rc.CurrentContext
	// }

	return
}

func getInClusterConfig() (config *rest.Config, defaultNs string, err error) {
	config, err = rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster configuration problem: %s", err)
	}

	data, err := os.ReadFile(kubeNamespaceFilePath)
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster configuration problem: cannot determine default kubernetes namespace: error reading %s: %s", kubeNamespaceFilePath, err)
	}
	defaultNs = string(data)

	return
}

// APIResourceList fetches lists of APIResource objects from cluster. It returns all preferred
// resources if apiVersion is empty. An array with one list is returned if apiVersion is valid.
//
// NOTE that fetching all preferred resources can give errors if there are non-working
// api controllers in cluster.
func (c *Client) APIResourceList(apiVersion string) (lists []*metav1.APIResourceList, err error) {
	lists, err = c.apiResourceList(apiVersion)
	if err != nil {
		fmt.Println("KUBEERR2", err, reflect.TypeOf(err), errors.Cause(err), reflect.TypeOf(errors.Unwrap(err)))

		if apierrors.IsNotFound(errors.Cause(err)) {
			fmt.Println("INVALIDATE LIST")
			c.cachedDiscovery.Invalidate()
			return c.apiResourceList(apiVersion)
		} else {
			fmt.Println("Unknown error type", reflect.TypeOf(err), reflect.TypeOf(errors.Cause(err)))
		}

		return nil, err
	}

	return lists, nil
}

func (c *Client) apiResourceList(apiVersion string) (lists []*metav1.APIResourceList, err error) {
	if apiVersion == "" {
		// Get all preferred resources.
		// Can return errors if api controllers are not available.
		switch c.discovery().(type) {
		case *fakediscovery.FakeDiscovery:
			// FakeDiscovery does not implement ServerPreferredResources method
			// lets return all possible resources, its better then nil
			_, res, err := c.discovery().ServerGroupsAndResources()
			return res, err

		default:
			return c.discovery().ServerPreferredResources()
		}
	} else {
		// Get only resources for desired group and version
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return nil, fmt.Errorf("apiVersion '%s' is invalid", apiVersion)
		}

		list, err := c.discovery().ServerResourcesForGroupVersion(gv.String())
		if err != nil {
			fmt.Println("KUBEERR1", reflect.TypeOf(err))
			return nil, errors.Wrapf(err, "apiVersion '%s' has no supported resources in cluster", apiVersion)
		}
		lists = []*metav1.APIResourceList{list}
	}

	return
}

// APIResource fetches APIResource object from cluster that specifies the name of a resource and whether it is namespaced.
//
// NOTE that fetching with empty apiVersion can give errors if there are non-working
// api controllers in cluster.
func (c *Client) APIResource(apiVersion, kind string) (res *metav1.APIResource, err error) {
	lists, err := c.APIResourceList(apiVersion)
	if err != nil && len(lists) == 0 {
		// apiVersion is defined and there is a ServerResourcesForGroupVersion error
		return nil, err
	}

	fmt.Println("KIND", kind)

	resource := getApiResourceFromResourceLists(kind, lists)
	if resource != nil {
		return resource, nil
	}
	fmt.Println("AFTER1", resource)

	//fmt.Println("INVALIDATE")
	//c.cachedDiscovery.Invalidate()
	//
	//resource = getApiResourceFromResourceLists(kind, lists)
	//if resource != nil {
	//	return resource, nil
	//}
	//
	//fmt.Println("AFTER2", resource)

	// If resource is not found, append additional error, may be the custom API of the resource is not available.
	additionalErr := ""
	if err != nil {
		additionalErr = fmt.Sprintf(", additional error: %s", err.Error())
	}
	err = fmt.Errorf("apiVersion '%s', kind '%s' is not supported by cluster%s", apiVersion, kind, additionalErr)
	return nil, err
}

// GroupVersionResource returns a GroupVersionResource object to use with dynamic informer.
//
// This method is borrowed from kubectl and kubedog. The difference are:
// - lower case comparison with kind, name and all short names
func (c *Client) GroupVersionResource(apiVersion, kind string) (gvr schema.GroupVersionResource, err error) {
	apiRes, err := c.APIResource(apiVersion, kind)
	if err != nil {
		return
	}

	return schema.GroupVersionResource{
		Resource: apiRes.Name,
		Group:    apiRes.Group,
		Version:  apiRes.Version,
	}, nil
}

func (c *Client) discovery() discovery.DiscoveryInterface {
	if c.cachedDiscovery != nil {
		return c.cachedDiscovery
	}
	return c.Discovery()
}

// InvalidateDiscoveryCache allows you to invalidate cache manually, for example, when you are deploying CRD
// KubeClient tries to invalidate cache automatically when needed, but you can save a few resources to call this manually
func (c *Client) InvalidateDiscoveryCache() {
	if c.cachedDiscovery != nil {
		c.cachedDiscovery.Invalidate()
	}
}

func equalLowerCasedToOneOf(term string, choices ...string) bool {
	if len(choices) == 0 {
		return false
	}
	lTerm := strings.ToLower(term)
	for _, choice := range choices {
		if lTerm == strings.ToLower(choice) {
			return true
		}
	}

	return false
}

func getApiResourceFromResourceLists(kind string, resourceLists []*metav1.APIResourceList) *metav1.APIResource {
	for _, list := range resourceLists {
		for _, resource := range list.APIResources {
			// TODO is it ok to ignore resources with no verbs?
			if len(resource.Verbs) == 0 {
				continue
			}

			if equalLowerCasedToOneOf(kind, append(resource.ShortNames, resource.Kind, resource.Name)...) {
				gv, _ := schema.ParseGroupVersion(list.GroupVersion)
				resource.Group = gv.Group
				resource.Version = gv.Version
				return &resource
			}
		}
	}

	return nil
}
