package client

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
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

	// load the gcp plugin (only required to authenticate against GKE clusters)
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/metrics"

	// route klog messages from client-go to logrus
	_ "github.com/flant/kube-client/klogtologrus"
)

const (
	kubeTokenFilePath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	kubeNamespaceFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

type Client interface {
	kubernetes.Interface

	WithContextName(contextName string)
	WithConfigPath(configPath string)
	WithServer(server string)
	WithRateLimiterSettings(qps float32, burst int)
	WithTimeout(time time.Duration)
	WithMetricStorage(metricStorage MetricStorage)
	WithMetricLabels(labels map[string]string)

	Init() error

	DefaultNamespace() string
	Dynamic() dynamic.Interface
	ApiExt() apixv1client.ApiextensionsV1Interface

	APIResourceList(apiVersion string) ([]*metav1.APIResourceList, error)
	APIResource(apiVersion, kind string) (*metav1.APIResource, error)
	GroupVersionResource(apiVersion, kind string) (schema.GroupVersionResource, error)
}

func New() Client {
	return &client{}
}

func NewFake(_ map[schema.GroupVersionResource]string) Client {
	scheme := runtime.NewScheme()
	objs := []runtime.Object{}

	return &client{
		Interface:        fake.NewSimpleClientset(),
		defaultNamespace: "default",
		dynamicClient:    fakedynamic.NewSimpleDynamicClient(scheme, objs...),
	}
}

var _ Client = &client{}

type client struct {
	kubernetes.Interface
	cachedDiscovery  discovery.CachedDiscoveryInterface
	contextName      string
	configPath       string
	defaultNamespace string
	dynamicClient    dynamic.Interface
	apiExtClient     apixv1client.ApiextensionsV1Interface
	qps              float32
	burst            int
	timeout          time.Duration
	server           string
	metricStorage    MetricStorage
	metricLabels     map[string]string
}

func (c *client) WithServer(server string) {
	c.server = server
}

func (c *client) WithContextName(name string) {
	c.contextName = name
}

func (c *client) WithConfigPath(path string) {
	c.configPath = path
}

func (c *client) WithRateLimiterSettings(qps float32, burst int) {
	c.qps = qps
	c.burst = burst
}

func (c *client) WithTimeout(timeout time.Duration) {
	c.timeout = timeout
}

func (c *client) WithMetricStorage(metricStorage MetricStorage) {
	c.metricStorage = metricStorage
}

func (c *client) WithMetricLabels(labels map[string]string) {
	c.metricLabels = labels
}

func (c *client) DefaultNamespace() string {
	return c.defaultNamespace
}

func (c *client) Dynamic() dynamic.Interface {
	return c.dynamicClient
}

func (c *client) ApiExt() apixv1client.ApiextensionsV1Interface {
	return c.apiExtClient
}

func (c *client) Init() error {
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
						} else {
							return fmt.Errorf("in-cluster config is not found")
						}
					} else {
						logEntry.Errorf("in-cluster problem: %s", err)
						return err
					}
				}
			} else {
				// if not in cluster return outOfCluster error
				if outOfClusterErr != nil {
					logEntry.Errorf("out-of-cluster problem: %s", outOfClusterErr)
					return outOfClusterErr
				} else {
					return fmt.Errorf("no kubernetes client config found")
				}
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

	if c.metricStorage != nil {
		metrics.Register(
			metrics.RegisterOpts{
				RequestLatency: NewRateLimiterLatencyMetric(c.metricStorage),
				RequestResult:  NewRequestResultMetric(c.metricStorage, c.metricLabels),
			},
		)
	}

	cacheDiscoveryDir, err := ioutil.TempDir("", "kube-cache-discovery-*")
	if err != nil {
		return err
	}

	cacheHttpDir, err := ioutil.TempDir("", "kube-cache-http-*")
	if err != nil {
		return err
	}

	c.cachedDiscovery, err = disk.NewCachedDiscoveryClientForConfig(config, cacheDiscoveryDir, cacheHttpDir, 10*time.Minute)
	if err != nil {
		return err
	}

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

	data, err := ioutil.ReadFile(kubeNamespaceFilePath)
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
func (c *client) APIResourceList(apiVersion string) (lists []*metav1.APIResourceList, err error) {
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
			return nil, fmt.Errorf("apiVersion '%s' has no supported resources in cluster: %v", apiVersion, err)
		}
		lists = []*metav1.APIResourceList{list}
	}

	// TODO should it copy group and version into each resource?

	// TODO create debug command to output this from cli
	// Debug mode will list all available CRDs for apiVersion
	// for _, r := range list.APIResources {
	//	log.Debugf("GVR: %30s %30s %30s", list.GroupVersion, r.Kind,
	//		fmt.Sprintf("%+v", append([]string{r.Name}, r.ShortNames...)),
	//	)
	// }

	return
}

// APIResource fetches APIResource object from cluster that specifies the name of a resource and whether it is namespaced.
//
// NOTE that fetching with empty apiVersion can give errors if there are non-working
// api controllers in cluster.
func (c *client) APIResource(apiVersion, kind string) (res *metav1.APIResource, err error) {
	lists, err := c.APIResourceList(apiVersion)
	if err != nil && len(lists) == 0 {
		// apiVersion is defined and there is a ServerResourcesForGroupVersion error
		return nil, err
	}

	resource := getApiResourceFromResourceLists(kind, lists)
	if resource != nil {
		return resource, nil
	}

	if c.cachedDiscovery != nil {
		c.cachedDiscovery.Invalidate()
	}

	resource = getApiResourceFromResourceLists(kind, lists)
	if resource != nil {
		return resource, nil
	}

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
func (c *client) GroupVersionResource(apiVersion, kind string) (gvr schema.GroupVersionResource, err error) {
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

func (c *client) discovery() discovery.DiscoveryInterface {
	if c.cachedDiscovery != nil {
		return c.cachedDiscovery
	}
	return c.Discovery()
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
