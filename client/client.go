package client

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/deckhouse/deckhouse/pkg/log"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
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
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientgometrics "k8s.io/client-go/tools/metrics"

	internalmetrics "github.com/flant/kube-client/internal/metrics"
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
		c.logger = log.NewLogger().Named("kubernetes-api-client").With("operator.component", "KubernetesAPIClient")
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

// IsWatchListSemanticsUnSupported forwards the WatchList opt-out of the embedded clientset
// (the fake one reports true) to client-go reflectors. Embedding kubernetes.Interface only
// promotes the interface methods, so without this client-go 0.35+ informers built on a fake
// Client wait forever for the initial-events bookmark.
func (c *Client) IsWatchListSemanticsUnSupported() bool {
	// A local assertion instead of watchlist.DoesClientNotSupportWatchListSemantics keeps
	// kube-client buildable with client-go < 0.35, where that helper does not exist.
	optOut, ok := c.Interface.(interface{ IsWatchListSemanticsUnSupported() bool })

	return ok && optOut.IsWatchListSemanticsUnSupported()
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
	metricPrefix     string
	schema           *runtime.Scheme
	restConfig       *rest.Config
	logger           *log.Logger
	// sfDiscovery wraps cachedDiscovery with GV-level singleflight deduplication
	// and mutex-protected Invalidate. All discovery calls go through this wrapper
	// so that every code path (APIResourceList, ToRESTMapper, ToDiscoveryClient)
	// is protected. Initialized lazily via sfDiscoveryOnce.
	sfDiscovery     *sfCachedDiscovery
	sfDiscoveryOnce sync.Once
	// discoverySF deduplicates concurrent calls at the apiVersion-request level
	// (key = "apiResourceList:"+apiVersion), sharing the returned
	// []*metav1.APIResourceList across goroutines that arrive during the same
	// in-flight request.
	discoverySF singleflight.Group
	// acceptOnlyJSONContentType
	// use only JSON for interactions with kube-api
	acceptOnlyJSONContentType bool
}

// ReloadDynamic replaces the fake dynamic client with a new one that knows the
// given set of CRDs. It does nothing for a Client connected to a real cluster.
func (c *Client) ReloadDynamic(gvrList map[schema.GroupVersionResource]string) {
	if _, ok := c.dynamicClient.(*fakedynamic.FakeDynamicClient); !ok {
		c.logger.Warn("ReloadDynamic is supported only for a fake client, ignoring")
		return
	}

	c.dynamicClient = fakedynamic.NewSimpleDynamicClientWithCustomListKinds(c.schema, gvrList)
}

// WithRestConfig sets a pre-configured rest.Config for the client
func (c *Client) WithRestConfig(config *rest.Config) {
	c.restConfig = config
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

func (c *Client) WithAcceptOnlyJSONContentType(e bool) {
	c.acceptOnlyJSONContentType = e
}

func (c *Client) WithLogger(logger *log.Logger) {
	if logger != nil {
		c.logger = logger
	}
}

func (c *Client) WithRateLimiterSettings(qps float32, burst int) {
	c.qps = qps
	c.burst = burst
}

func (c *Client) WithTimeout(timeout time.Duration) {
	c.timeout = timeout
}

// WithMetricStorage sets the metric storage backend used to record Kubernetes
// client metrics. Call WithMetricLabels and WithMetricPrefix before Init to
// customise dimensions and the metric name prefix.
func (c *Client) WithMetricStorage(storage MetricStorage) {
	c.metricStorage = storage
}

// WithMetricLabels sets extra label key/value pairs that are attached to every
// metric sample emitted by this client.
func (c *Client) WithMetricLabels(labels map[string]string) {
	c.metricLabels = labels
}

// WithMetricPrefix sets the prefix that replaces the {PREFIX} placeholder in
// all Kubernetes client metric names (see internal/metrics). An empty prefix
// removes the placeholder, making the metric names start with "kubernetes_".
func (c *Client) WithMetricPrefix(prefix string) {
	c.metricPrefix = prefix
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

const defaultMetricPrefix = "kube_client_"

func (c *Client) Init() error {
	if c.logger == nil {
		c.logger = log.NewLogger().Named("kubernetes-api-client").With("operator.component", "KubernetesAPIClient")
	}

	if c.metricStorage == nil {
		c.metricStorage = newDefaultMetricStorage()
	}

	if c.metricPrefix == "" {
		c.metricPrefix = defaultMetricPrefix
	}

	var (
		err    error
		config *rest.Config
	)

	configType := "out-of-cluster"

	var defaultNs string

	switch {
	case c.restConfig != nil:
		if c.restConfig.Host == "" {
			return fmt.Errorf("rest config host can't be empty")
		}

		// Copy so the caller's config is not changed by the settings applied below.
		config = rest.CopyConfig(c.restConfig)
		defaultNs = "default"
		configType = "rest-config"
	case c.server == "":
		// Try to load from kubeconfig in flags or from ~/.kube/config
		var outOfClusterErr error

		config, defaultNs, outOfClusterErr = getOutOfClusterConfig(c.contextName, c.configPath)

		if config == nil {
			if outOfClusterErr == nil {
				outOfClusterErr = fmt.Errorf("no kubernetes client config found")
			}

			// An explicitly requested kubeconfig or context must fail loudly:
			// falling back to the in-cluster config would talk to another cluster.
			if c.configPath != "" || c.contextName != "" || !hasInClusterConfig() {
				c.logger.Error("out-of-cluster problem", slog.String("error", outOfClusterErr.Error()))
				return outOfClusterErr
			}

			config, defaultNs, err = getInClusterConfig()
			if err != nil {
				c.logger.Error("in-cluster problem", slog.String("error", err.Error()))
				return err
			}

			configType = "in-cluster"
		}
	default:
		// use specific server to connect to API
		config = &rest.Config{
			Host: c.server,
		}
		_ = rest.SetKubernetesDefaults(config)
		defaultNs = "default"
		configType = "server"
	}

	if config == nil {
		return fmt.Errorf("failed to initialize kubernetes client: no valid configuration found")
	}

	if c.acceptOnlyJSONContentType {
		config.AcceptContentTypes = "application/json"
	}

	c.defaultNamespace = defaultNs

	if c.qps != 0 {
		config.QPS = c.qps
	}

	if c.burst != 0 {
		config.Burst = c.burst
	}

	if c.timeout != 0 {
		config.Timeout = c.timeout
	}

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

	internalmetrics.RegisterKubernetesClientMetrics(c.metricStorage, c.metricLabels, c.metricPrefix)
	// client-go accepts metric backends once per process: only the first
	// initialized Client wires its storage, prefix and labels into client-go.
	clientgometrics.Register(clientgometrics.RegisterOpts{
		RequestLatency:     internalmetrics.NewRequestLatency(c.metricStorage, c.metricLabels, c.metricPrefix),
		RateLimiterLatency: internalmetrics.NewRateLimiterLatency(c.metricStorage, c.metricLabels, c.metricPrefix),
		RequestResult:      internalmetrics.NewRequestResult(c.metricStorage, c.metricLabels, c.metricPrefix),
	})

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

// Disk cache directories are created once per process and shared by every
// Client, so repeated Init calls do not leave new directories behind.
var (
	diskCacheDirsOnce  sync.Once
	diskCacheDiscovery string
	diskCacheHTTP      string
	diskCacheDirsErr   error
)

func newDiskCachedDiscovery(config *rest.Config) (*disk.CachedDiscoveryClient, error) {
	diskCacheDirsOnce.Do(func() {
		diskCacheDiscovery, diskCacheDirsErr = os.MkdirTemp("", "kube-cache-discovery-*")
		if diskCacheDirsErr != nil {
			return
		}

		diskCacheHTTP, diskCacheDirsErr = os.MkdirTemp("", "kube-cache-http-*")
	})

	if diskCacheDirsErr != nil {
		return nil, diskCacheDirsErr
	}

	// Discovery files are not keyed by host, so each API server gets its own
	// subdirectory. HTTP cache entries are keyed by full URL and can be shared.
	discoveryDir := filepath.Join(diskCacheDiscovery, url.PathEscape(config.Host))

	cachedDiscovery, err := disk.NewCachedDiscoveryClientForConfig(config, discoveryDir, diskCacheHTTP, 10*time.Minute)
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

func getOutOfClusterConfig(contextName, configPath string) (*rest.Config, string, error) {
	clientConfig := getClientConfig(contextName, configPath)

	defaultNs, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("cannot determine default kubernetes namespace: %s", err)
	}

	config, err := clientConfig.ClientConfig()
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

	return config, defaultNs, nil
}

func getInClusterConfig() (*rest.Config, string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster configuration problem: %s", err)
	}

	data, err := os.ReadFile(kubeNamespaceFilePath)
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster configuration problem: cannot determine default kubernetes namespace: error reading %s: %s", kubeNamespaceFilePath, err)
	}

	return config, string(data), nil
}

// APIResourceList fetches lists of APIResource objects from cluster. It returns all preferred
// resources if apiVersion is empty. An array with one list is returned if apiVersion is valid.
//
// The returned slice and its *metav1.APIResourceList elements may be shared
// across goroutines that joined the same singleflight call. Callers must treat
// the result as read-only and must not modify the returned objects.
//
// NOTE that fetching all preferred resources can give errors if there are non-working
// api controllers in cluster.
func (c *Client) APIResourceList(apiVersion string) ([]*metav1.APIResourceList, error) {
	lists, err := c.apiResourceList(apiVersion)
	// *errors.errorString type is here, we can't check it another way
	if err == nil || !strings.Contains(err.Error(), "not found") {
		return lists, err
	}

	if apiVersion == "" {
		c.invalidateDiscovery()
		return c.apiResourceList(apiVersion)
	}

	// The group version may have appeared after the cache was filled, for
	// example a CRD installed by a parallel hook. Ask the server for this one
	// group version instead of dropping the whole cache on every miss.
	list, freshErr := c.serverResourcesForGroupVersion(apiVersion)
	if freshErr != nil {
		return lists, err
	}

	c.invalidateDiscovery()

	return []*metav1.APIResourceList{list}, nil
}

// serverResourcesForGroupVersion asks the API server for one group version,
// bypassing the discovery cache. It costs one small request, so callers that
// wait for a CRD to appear can call it on every lookup miss.
func (c *Client) serverResourcesForGroupVersion(apiVersion string) (*metav1.APIResourceList, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, fmt.Errorf("apiVersion '%s' is invalid", apiVersion)
	}

	list, err := c.Discovery().ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		return nil, err
	}

	if list == nil {
		return nil, apiErrors.NewNotFound(schema.GroupResource{Group: gv.Group}, gv.Version)
	}

	return list, nil
}

// sfCachedDiscovery wraps a CachedDiscoveryInterface and deduplicates concurrent
// calls to ServerResourcesForGroupVersion through an internal singleflight group,
// using the group-version string as the key.  This prevents the upstream disk
// cache from encoding the same shared *metav1.APIResourceList from multiple
// goroutines simultaneously (writeCachedFile → runtime.Encode mutates TypeMeta
// without holding a lock).
//
// ServerPreferredResources and ServerGroupsAndResources are overridden to route
// each group version through our ServerResourcesForGroupVersion on non-aggregated
// servers. For servers that implement AggregatedDiscoveryInterface the underlying
// client is used directly to preserve the single-round-trip performance benefit
// (aggregated discovery does not call ServerResourcesForGroupVersion per GV).
//
// Invalidate is mutex-protected because the disk-backed implementation is not
// goroutine-safe.
type sfCachedDiscovery struct {
	discovery.CachedDiscoveryInterface
	sf singleflight.Group
	mu sync.Mutex
}

func (s *sfCachedDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	v, err, _ := s.sf.Do(groupVersion, func() (any, error) {
		return s.CachedDiscoveryInterface.ServerResourcesForGroupVersion(groupVersion)
	})
	if v == nil {
		return nil, err
	}

	return v.(*metav1.APIResourceList), err
}

// ServerPreferredResources routes through the package-level helper with s as
// receiver so that each group version's disk write is serialized by our
// singleflight. Aggregated-discovery servers are delegated directly to avoid
// forcing N per-GV API calls when one round trip suffices.
func (s *sfCachedDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	if _, ok := s.CachedDiscoveryInterface.(discovery.AggregatedDiscoveryInterface); ok {
		return s.CachedDiscoveryInterface.ServerPreferredResources()
	}

	return discovery.ServerPreferredResources(s)
}

// ServerGroupsAndResources routes through the package-level helper with s as
// receiver for non-aggregated servers. See ServerPreferredResources.
func (s *sfCachedDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	if _, ok := s.CachedDiscoveryInterface.(discovery.AggregatedDiscoveryInterface); ok {
		return s.CachedDiscoveryInterface.ServerGroupsAndResources()
	}

	return discovery.ServerGroupsAndResources(s)
}

func (s *sfCachedDiscovery) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.CachedDiscoveryInterface.Invalidate()
}

func (c *Client) initSFDiscovery() *sfCachedDiscovery {
	c.sfDiscoveryOnce.Do(func() {
		if c.cachedDiscovery != nil {
			c.sfDiscovery = &sfCachedDiscovery{CachedDiscoveryInterface: c.cachedDiscovery}
		}
	})

	return c.sfDiscovery
}

func (c *Client) apiResourceList(apiVersion string) ([]*metav1.APIResourceList, error) {
	// Deduplicate concurrent calls for the same apiVersion. This prevents
	// redundant apiResourceListUncached calls and shares the full
	// []*metav1.APIResourceList result across all goroutines that joined the
	// same in-flight request.
	//
	// GV-level deduplication (preventing concurrent disk writes for the same
	// group version across different call paths) is handled inside sfCachedDiscovery.
	v, err, _ := c.discoverySF.Do("apiResourceList:"+apiVersion, func() (any, error) {
		return c.apiResourceListUncached(apiVersion)
	})
	if v == nil {
		return nil, err
	}

	return v.([]*metav1.APIResourceList), err
}

func (c *Client) apiResourceListUncached(apiVersion string) ([]*metav1.APIResourceList, error) {
	if apiVersion == "" {
		// Get all preferred resources.
		// Can return errors if api controllers are not available.
		//
		// Determine the raw discovery type without the sfCachedDiscovery wrapper.
		// When c.cachedDiscovery is nil the client falls back to c.Interface's
		// discovery, which may be a *fakediscovery.FakeDiscovery.
		var rawDisc discovery.DiscoveryInterface
		if c.cachedDiscovery != nil {
			rawDisc = c.cachedDiscovery
		} else {
			rawDisc = c.Discovery()
		}

		switch rawDisc.(type) {
		case *fakediscovery.FakeDiscovery:
			// FakeDiscovery does not implement ServerPreferredResources method
			// lets return all possible resources, its better then nil
			_, res, err := c.discovery().ServerGroupsAndResources()
			return res, err

		default:
			return c.discovery().ServerPreferredResources()
		}
	}

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

	return []*metav1.APIResourceList{list}, nil
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
			resource, err = c.apiResourceFromServer(apiVersion, kind)
		} else {
			return nil, fmt.Errorf("apiVersion '%s', kind '%s' is not supported by cluster: %w", apiVersion, kind, err)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("apiVersion '%s', kind '%s' is not supported by cluster: %w", apiVersion, kind, err)
	}

	return resource, nil
}

// apiResourceFromServer looks a kind up after a cache miss. The resource may
// be on its way, for example a CRD installed by a parallel hook, so the lookup
// goes to the server. With apiVersion set it is one request for that group
// version, and the cache is invalidated only when the kind is really there.
func (c *Client) apiResourceFromServer(apiVersion, kind string) (*metav1.APIResource, error) {
	if apiVersion == "" {
		c.invalidateDiscovery()
		return c.apiResource(apiVersion, kind)
	}

	list, err := c.serverResourcesForGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}

	resource := getApiResourceFromResourceLists(kind, []*metav1.APIResourceList{list})
	if resource == nil {
		gv, _ := schema.ParseGroupVersion(apiVersion)
		return nil, apiErrors.NewNotFound(schema.GroupResource{Group: gv.Group, Resource: kind}, "")
	}

	c.invalidateDiscovery()

	return resource, nil
}

func (c *Client) apiResource(apiVersion, kind string) (*metav1.APIResource, error) {
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
func (c *Client) GroupVersionResource(apiVersion, kind string) (schema.GroupVersionResource, error) {
	apiRes, err := c.APIResource(apiVersion, kind)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}

	return schema.GroupVersionResource{
		Resource: apiRes.Name,
		Group:    apiRes.Group,
		Version:  apiRes.Version,
	}, nil
}

func (c *Client) discovery() discovery.DiscoveryInterface {
	if d := c.initSFDiscovery(); d != nil {
		return d
	}

	return c.Discovery()
}

// InvalidateDiscoveryCache allows you to invalidate cache manually, for example, when you are deploying CRD
// KubeClient tries to invalidate cache automatically when needed, but you can save a few resources to call this manually
func (c *Client) InvalidateDiscoveryCache() {
	c.invalidateDiscovery()
}

// invalidateDiscovery resets the cached discovery state so that subsequent
// calls fetch fresh data from the API server. It is goroutine-safe.
func (c *Client) invalidateDiscovery() {
	if d := c.initSFDiscovery(); d != nil {
		d.Invalidate() // sfCachedDiscovery.Invalidate holds its own mutex
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

// ToXXX functions implements https://pkg.go.dev/k8s.io/cli-runtime/pkg/genericclioptions#RESTClientGetter interface

func (c *Client) ToRESTConfig() (*rest.Config, error) {
	return c.restConfig, nil
}

// ToDiscoveryClient returns the discovery client wrapped in sfCachedDiscovery,
// which serializes concurrent ServerResourcesForGroupVersion calls through a
// singleflight group (preventing disk-encoding races) and protects Invalidate
// with a mutex.
func (c *Client) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	if d := c.initSFDiscovery(); d != nil {
		return d, nil
	}

	return c.cachedDiscovery, nil
}

func (c *Client) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return getClientConfig(c.contextName, c.configPath)
}

// ToRESTMapper returns a RESTMapper backed by sfCachedDiscovery so that mapper
// calls to ServerResourcesForGroupVersion are deduplicated by the same
// singleflight group used by APIResourceList, preventing disk-encoding races.
func (c *Client) ToRESTMapper() (meta.RESTMapper, error) {
	disc := c.cachedDiscovery
	if d := c.initSFDiscovery(); d != nil {
		disc = d
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(disc)
	expander := restmapper.NewShortcutExpander(mapper, disc,
		func(warning string) {
			c.logger.Warn("warning", slog.String("warning", warning))
		})

	return expander, nil
}

func (c *Client) NewBuilder() *resource.Builder {
	return resource.NewBuilder(c)
}

// IsWatchListSemanticsUnSupported proxies the capability check to the wrapped clientset.
//
// client-go 0.35 enables the WatchListClient feature gate by default. Its fake clientset
// does not implement WatchList and reports that through this method, but the method is not
// part of kubernetes.Interface, so embedding hides it. Without this proxy the reflector
// keeps WatchList enabled against a fake client and never syncs.
func (c *Client) IsWatchListSemanticsUnSupported() bool {
	u, ok := c.Interface.(interface{ IsWatchListSemanticsUnSupported() bool })
	if !ok {
		return false
	}

	return u.IsWatchListSemanticsUnSupported()
}
