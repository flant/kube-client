package client

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/deckhouse/deckhouse/pkg/log"
	"github.com/pkg/errors"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/discovery/cached/memory"
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

	_ "github.com/flant/kube-client/klogtolog" // route klog messages from client-go to log
)

const (
	kubeTokenFilePath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	kubeNamespaceFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

type Option func(client *Client)

func WithLogger(logger *log.Logger) Option {
	return func(client *Client) {
		client.logger = logger.With("operator.component", "KubernetesAPIClient")
	}
}

// TODO: refactor all "with" methods
func New(opts ...Option) *Client {
	c := &Client{}

	for _, fn := range opts {
		fn(c)
	}

	if c.logger == nil {
		c.logger = log.NewLogger(log.Options{}).Named("kubernetes-api-client").With("operator.component", "KubernetesAPIClient")
	}

	return c
}

func NewFake(gvr map[schema.GroupVersionResource]string) *Client {
	sc := runtime.NewScheme()
	return &Client{
		Interface:        fake.NewSimpleClientset(),
		defaultNamespace: "default",
		dynamicClient:    fakedynamic.NewSimpleDynamicClientWithCustomListKinds(sc, gvr),
		metadataClient:   fakemetadata.NewSimpleMetadataClient(sc),
		schema:           sc,
		logger:           log.NewNop(),
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
	logger           *log.Logger
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
	if c.logger == nil {
		c.logger = log.NewLogger(log.Options{}).Named("kubernetes-api-client").With("operator.component", "KubernetesAPIClient")
	}

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
							c.logger.Error("configuration problems", slog.String("error", err.Error()))
							return err
						}
						return fmt.Errorf("in-cluster config is not found")
					}
					c.logger.Error("in-cluster problem", slog.String("error", err.Error()))
					return err
				}
			} else {
				// if not in cluster return outOfCluster error
				if outOfClusterErr != nil {
					c.logger.Error("out-of-cluster problem", slog.String("error", outOfClusterErr.Error()))
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
		c.logger.Error("configuration problem", slog.String("error", err.Error()))
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

	if _, fd := os.LookupEnv(`FLANT_KUBE_CLIENT_IN_MEMORY_DISCOVERY_CACHE`); fd {
		discovery, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return err
		}
		c.cachedDiscovery = memory.NewMemCacheClient(discovery)
	} else {
		c.cachedDiscovery, err = newDiskCachedDiscovery(config)
		if err != nil {
			return err
		}
	}

	c.restConfig = config
	c.logger.Debug("Kubernetes client is configured successfully with config", slog.String("config", configType))

	return nil
}

func newDiskCachedDiscovery(config *rest.Config) (*disk.CachedDiscoveryClient, error) {
	cacheDiscoveryDir, err := os.MkdirTemp("", "kube-cache-discovery-*")
	if err != nil {
		return nil, err
	}

	cacheHttpDir, err := os.MkdirTemp("", "kube-cache-http-*")
	if err != nil {
		return nil, err
	}

	cachedDiscovery, err := disk.NewCachedDiscoveryClientForConfig(config, cacheDiscoveryDir, cacheHttpDir, 10*time.Minute)
	if err != nil {
		return nil, err
	}

	return cachedDiscovery, nil
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
		if strings.Contains(err.Error(), "not found") {
			// *errors.errorString type is here, we can't check it another way
			c.cachedDiscovery.Invalidate()
			return c.apiResourceList(apiVersion)
		}

		return lists, err
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
			// if not found, err has type *errors.errorString here
			return nil, errors.Wrapf(err, "apiVersion '%s' has no supported resources in cluster", apiVersion)
		}
		lists = []*metav1.APIResourceList{list}
	}

	return
}

// APIResource fetches APIResource object from cluster that specifies the name of a resource and whether it is namespaced.
// if resource not found, we try to invalidate cache and
//
// NOTE that fetching with empty apiVersion can give errors if there are non-working
// api controllers in cluster.
func (c *Client) APIResource(apiVersion, kind string) (*metav1.APIResource, error) {
	resource, err := c.apiResource(apiVersion, kind)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			c.cachedDiscovery.Invalidate()
			resource, err = c.apiResource(apiVersion, kind)
		} else {
			return nil, fmt.Errorf("apiVersion '%s', kind '%s' is not supported by cluster: %w", apiVersion, kind, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("apiVersion '%s', kind '%s' is not supported by cluster: %w", apiVersion, kind, err)
	}

	return resource, nil
}

func (c *Client) apiResource(apiVersion, kind string) (res *metav1.APIResource, err error) {
	lists, err := c.APIResourceList(apiVersion)
	if err != nil && len(lists) == 0 {
		// apiVersion is defined and there is a ServerResourcesForGroupVersion error
		return nil, err
	}

	resource := getApiResourceFromResourceLists(kind, lists)
	if resource == nil {
		gv, _ := schema.ParseGroupVersion(apiVersion)
		return nil, apiErrors.NewNotFound(schema.GroupResource{Group: gv.Group, Resource: kind}, "")
	}

	return resource, nil
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
