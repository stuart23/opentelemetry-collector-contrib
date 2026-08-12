// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package k8sclusterreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver"

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	quotaclientset "github.com/openshift/client-go/quota/clientset/versioned"
	quotainformersv1 "github.com/openshift/client-go/quota/informers/externalversions"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/k8sconfig"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/cronjob"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/customresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/daemonset"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/deployment"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/gvk"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/hpa"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/jobs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/namespace"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/node"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/persistentvolume"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/persistentvolumeclaim"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/pod"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/replicaset"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/replicationcontroller"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/service"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/statefulset"
)

type sharedInformer interface {
	Start(<-chan struct{})
	WaitForCacheSync(<-chan struct{}) map[reflect.Type]bool
}

type resourceWatcher struct {
	client              kubernetes.Interface
	osQuotaClient       quotaclientset.Interface
	dynamicClient       dynamic.Interface
	informerFactories   []sharedInformer
	metadataStore       *metadata.Store
	logger              *zap.Logger
	metadataConsumers   []metadataConsumer
	initialTimeout      time.Duration
	initialSyncDone     *atomic.Bool
	initialSyncTimedOut *atomic.Bool
	config              *Config
	entityLogConsumer   consumer.Logs

	// For mocking.
	makeClient               func(apiConf k8sconfig.APIConfig) (kubernetes.Interface, error)
	makeOpenShiftQuotaClient func(apiConf k8sconfig.APIConfig) (quotaclientset.Interface, error)
	makeDynamicClient        func(apiConf k8sconfig.APIConfig) (dynamic.Interface, error)
}

type metadataConsumer func(metadata []*experimentalmetricmetadata.MetadataUpdate) error

// newResourceWatcher creates a Kubernetes resource watcher.
func newResourceWatcher(set receiver.Settings, cfg *Config, metadataStore *metadata.Store) *resourceWatcher {
	return &resourceWatcher{
		logger:                   set.Logger,
		metadataStore:            metadataStore,
		initialSyncDone:          &atomic.Bool{},
		initialSyncTimedOut:      &atomic.Bool{},
		initialTimeout:           defaultInitialSyncTimeout,
		config:                   cfg,
		makeClient:               k8sconfig.MakeClient,
		makeOpenShiftQuotaClient: k8sconfig.MakeOpenShiftQuotaClient,
		makeDynamicClient:        k8sconfig.MakeDynamicClient,
	}
}

func (rw *resourceWatcher) initialize() error {
	client, err := rw.makeClient(rw.config.APIConfig)
	if err != nil {
		return fmt.Errorf("Failed to create Kubernetes client: %w", err)
	}
	rw.client = client

	if rw.config.Distribution == distributionOpenShift && rw.config.Namespace == "" && len(rw.config.Namespaces) == 0 {
		rw.osQuotaClient, err = rw.makeOpenShiftQuotaClient(rw.config.APIConfig)
		if err != nil {
			return fmt.Errorf("Failed to create OpenShift quota API client: %w", err)
		}
	}

	if len(rw.config.Resources) > 0 {
		rw.dynamicClient, err = rw.makeDynamicClient(rw.config.APIConfig)
		if err != nil {
			return fmt.Errorf("Failed to create Kubernetes dynamic client: %w", err)
		}
		if err = rw.resolveCustomResourceVersions(); err != nil {
			return fmt.Errorf("Failed to resolve custom resource versions: %w", err)
		}
	}

	err = rw.prepareSharedInformerFactory()
	if err != nil {
		return err
	}

	return nil
}

// shouldWatchResourceForMetadataOnly returns true if a resource should be watched even though
// its metrics are disabled. This is the case when metadata exporters are configured or
// entity events are enabled (entityLogConsumer is set).
func (rw *resourceWatcher) shouldWatchResourceForMetadataOnly() bool {
	return len(rw.config.MetadataExporters) > 0 || rw.entityLogConsumer != nil
}

func (rw *resourceWatcher) prepareSharedInformerFactory() error {
	factories := rw.getInformerFactories()

	// Map of supported group version kinds by name of a kind.
	// If none of the group versions are supported by k8s server for a specific kind,
	// informer for that kind won't be set and a warning message is thrown.
	// This map should be kept in sync with what can be provided by the supported k8s server versions.
	supportedKinds := map[string][]schema.GroupVersionKind{
		"Pod":                     {gvk.Pod},
		"Node":                    {gvk.Node},
		"Namespace":               {gvk.Namespace},
		"ReplicationController":   {gvk.ReplicationController},
		"ResourceQuota":           {gvk.ResourceQuota},
		"Service":                 {gvk.Service},
		"DaemonSet":               {gvk.DaemonSet},
		"Deployment":              {gvk.Deployment},
		"ReplicaSet":              {gvk.ReplicaSet},
		"StatefulSet":             {gvk.StatefulSet},
		"Job":                     {gvk.Job},
		"CronJob":                 {gvk.CronJob},
		"HorizontalPodAutoscaler": {gvk.HorizontalPodAutoscaler},
	}

	// Only watch EndpointSlice if any service endpoint metrics are enabled
	if rw.shouldWatchEndpointSlice() {
		supportedKinds["EndpointSlice"] = []schema.GroupVersionKind{gvk.EndpointSlice}
	}

	// Resources with all metrics disabled by default.
	// Only watch these if any of their metrics are explicitly enabled or if metadata/entity destinations are configured.
	if rw.shouldWatchPersistentVolume() {
		supportedKinds["PersistentVolume"] = []schema.GroupVersionKind{gvk.PersistentVolume}
	}
	if rw.shouldWatchPersistentVolumeClaim() {
		supportedKinds["PersistentVolumeClaim"] = []schema.GroupVersionKind{gvk.PersistentVolumeClaim}
	}

	for kind, gvks := range supportedKinds {
		anySupported := false
		for _, gvk := range gvks {
			supported, err := rw.isKindSupported(gvk)
			if err != nil {
				return err
			}
			if supported {
				anySupported = true
				rw.setupInformerForKind(gvk, factories)
			}
		}
		if !anySupported {
			rw.logger.Warn("Server doesn't support any of the group versions defined for the kind",
				zap.String("kind", kind))
		}
	}

	if rw.osQuotaClient != nil {
		quotaFactory := quotainformersv1.NewSharedInformerFactory(rw.osQuotaClient, 0)
		rw.setupInformer(gvk.ClusterResourceQuota, metadata.ClusterWideInformerKey, quotaFactory.Quota().V1().ClusterResourceQuotas().Informer())
		rw.informerFactories = append(rw.informerFactories, quotaFactory)
	}
	for _, factory := range factories {
		rw.informerFactories = append(rw.informerFactories, factory)
	}

	rw.setupCustomResourceInformers()

	return nil
}

// resolveCustomResourceVersions fills in Version for any configured custom resource
// that didn't specify one, using the server's preferred version for that
// Group/Resource - mirroring the discovery-based resolution k8sobjectsreceiver's
// `objects` config already does. Only the preferred version is ever resolved: a CRD
// that serves several versions still has exactly one underlying set of stored
// objects, so watching more than one version would double-count the same resources.
// Entries whose Group/Resource can't be found are left with an empty Version and
// skipped (with a warning) by setupCustomResourceInformers.
func (rw *resourceWatcher) resolveCustomResourceVersions() error {
	anyUnresolved := false
	for _, cfg := range rw.config.Resources {
		if cfg.Version == "" {
			anyUnresolved = true
			break
		}
	}
	if !anyUnresolved {
		return nil
	}

	preferred, err := rw.client.Discovery().ServerPreferredResources()
	if preferred == nil && err != nil {
		return fmt.Errorf("failed to fetch preferred server resources: %w", err)
	}

	for i, cfg := range rw.config.Resources {
		if cfg.Version != "" {
			continue
		}
		version, found := findPreferredVersion(preferred, cfg.Group, cfg.Resource)
		if !found {
			rw.logger.Warn("Could not resolve a version for configured custom resource; it will not be watched",
				zap.String("group", cfg.Group), zap.String("resource", cfg.Resource))
			continue
		}
		rw.config.Resources[i].Version = version
	}
	return nil
}

// findPreferredVersion returns the version of the given group/resource in the
// server's preferred-resources list, as returned by ServerPreferredResources.
func findPreferredVersion(preferred []*metav1.APIResourceList, group, resource string) (string, bool) {
	for _, list := range preferred {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil || gv.Group != group {
			continue
		}
		for i := range list.APIResources {
			if list.APIResources[i].Name == resource {
				return gv.Version, true
			}
		}
	}
	return "", false
}

// getDynamicInformerFactories creates the dynamic informer factories used to watch
// the custom resources configured via Config.Resources. It mirrors getInformerFactories,
// but for the dynamic client, since arbitrary CRDs aren't known to the typed
// informers.SharedInformerFactory.
func (rw *resourceWatcher) getDynamicInformerFactories() map[string]dynamicinformer.DynamicSharedInformerFactory {
	factories := map[string]dynamicinformer.DynamicSharedInformerFactory{}

	switch {
	case len(rw.config.Namespaces) > 0:
		for _, ns := range rw.config.Namespaces {
			factories[ns] = dynamicinformer.NewFilteredDynamicSharedInformerFactory(rw.dynamicClient, rw.config.MetadataCollectionInterval, ns, nil)
		}
	case rw.config.Namespace != "":
		factories[rw.config.Namespace] = dynamicinformer.NewFilteredDynamicSharedInformerFactory(rw.dynamicClient, rw.config.MetadataCollectionInterval, rw.config.Namespace, nil)
	default:
		factories[metadata.ClusterWideInformerKey] = dynamicinformer.NewFilteredDynamicSharedInformerFactory(rw.dynamicClient, rw.config.MetadataCollectionInterval, metav1.NamespaceAll, nil)
	}

	return factories
}

// setupCustomResourceInformers sets up one dynamic informer per configured custom
// resource, per namespace being observed. Custom resources are represented as
// *unstructured.Unstructured, so this is the one generic path that serves every
// configured CRD, rather than a per-kind case as built-in kinds get in
// setupInformerForKind.
func (rw *resourceWatcher) setupCustomResourceInformers() {
	if len(rw.config.Resources) == 0 {
		return
	}

	dynFactories := rw.getDynamicInformerFactories()

	for _, cfg := range rw.config.Resources {
		if cfg.Version == "" {
			// Version resolution failed for this entry; already warned in
			// resolveCustomResourceVersions.
			continue
		}
		for ns, factory := range dynFactories {
			informer := factory.ForResource(cfg.GroupVersionResource()).Informer()
			rw.setupCustomResourceInformer(cfg, ns, informer)
		}
	}

	for _, factory := range dynFactories {
		rw.informerFactories = append(rw.informerFactories, dynamicInformerFactoryAdapter{factory})
	}
}

// setupCustomResourceInformer adds event handlers to a custom resource informer
// and sets up a metadataStore. It's a variant of setupInformer that closes over the
// resource's CustomResourceConfig, since - unlike built-in kinds - the object's Go
// type (*unstructured.Unstructured) can't disambiguate which of possibly several
// configured custom resources an incoming object belongs to.
func (rw *resourceWatcher) setupCustomResourceInformer(cfg metadata.CustomResourceConfig, namespace string, informer cache.SharedIndexInformer) {
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			rw.waitForInitialInformerSync()
			if !rw.hasDestination() {
				return
			}
			rw.syncMetadataUpdate(
				map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata{},
				customresource.GetMetadata(obj.(*unstructured.Unstructured), cfg),
			)
		},
		UpdateFunc: func(oldObj, newObj any) {
			rw.waitForInitialInformerSync()
			if !rw.hasDestination() {
				return
			}
			rw.syncMetadataUpdate(
				customresource.GetMetadata(oldObj.(*unstructured.Unstructured), cfg),
				customresource.GetMetadata(newObj.(*unstructured.Unstructured), cfg),
			)
		},
		DeleteFunc: func(oldObj any) {
			rw.waitForInitialInformerSync()
			if !rw.hasDestination() {
				return
			}
			rw.syncMetadataUpdate(
				customresource.GetMetadata(oldObj.(*unstructured.Unstructured), cfg),
				map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata{},
			)
		},
	})
	if err != nil {
		rw.logger.Error("error adding event handler to custom resource informer", zap.Error(err))
	}
	rw.metadataStore.Setup(cfg.GroupVersionKind(), namespace, informer.GetStore())
}

// dynamicInformerFactoryAdapter adapts a dynamicinformer.DynamicSharedInformerFactory
// to the sharedInformer interface used by startWatchingResources. The two factory
// types are otherwise identical, differing only in the return type of
// WaitForCacheSync, which startWatchingResources discards anyway.
type dynamicInformerFactoryAdapter struct {
	factory dynamicinformer.DynamicSharedInformerFactory
}

func (d dynamicInformerFactoryAdapter) Start(stopCh <-chan struct{}) {
	d.factory.Start(stopCh)
}

func (d dynamicInformerFactoryAdapter) WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool {
	d.factory.WaitForCacheSync(stopCh)
	return nil
}

// shouldWatchEndpointSlice returns true if the service endpoint count metric is enabled
func (rw *resourceWatcher) shouldWatchEndpointSlice() bool {
	return rw.config.MetricsBuilderConfig.Metrics.K8sServiceEndpointCount.Enabled
}

// shouldWatchPersistentVolume returns true if any PV metric is enabled or metadata/entity destinations are configured.
func (rw *resourceWatcher) shouldWatchPersistentVolume() bool {
	return rw.config.MetricsBuilderConfig.Metrics.K8sPersistentvolumeStatusPhase.Enabled ||
		rw.config.MetricsBuilderConfig.Metrics.K8sPersistentvolumeStorageCapacity.Enabled ||
		rw.shouldWatchResourceForMetadataOnly()
}

// shouldWatchPersistentVolumeClaim returns true if any PVC metric is enabled or metadata/entity destinations are configured.
func (rw *resourceWatcher) shouldWatchPersistentVolumeClaim() bool {
	return rw.config.MetricsBuilderConfig.Metrics.K8sPersistentvolumeclaimStatusPhase.Enabled ||
		rw.config.MetricsBuilderConfig.Metrics.K8sPersistentvolumeclaimStorageCapacity.Enabled ||
		rw.config.MetricsBuilderConfig.Metrics.K8sPersistentvolumeclaimStorageRequest.Enabled ||
		rw.shouldWatchResourceForMetadataOnly()
}

// getInformerFactories creates the informer factories which are used to set up the informers for the
// resources that should be observed. The informer factories are returned as a map[string]informer.SharedInformerFactory,
// where the map keys represent the namespace that should be observed. If the factory is created for the whole cluster,
// the factory is stored under an empty string
func (rw *resourceWatcher) getInformerFactories() map[string]informers.SharedInformerFactory {
	factories := map[string]informers.SharedInformerFactory{}

	switch {
	case len(rw.config.Namespaces) > 0:
		rw.logger.Info("Namespaces filter has been enabled. Nodes and namespaces will not be observed.", zap.String("namespaces", strings.Join(rw.config.Namespaces, ",")))
		for _, ns := range rw.config.Namespaces {
			factories[ns] = informers.NewSharedInformerFactoryWithOptions(
				rw.client,
				rw.config.MetadataCollectionInterval,
				informers.WithNamespace(ns),
			)
		}
	case rw.config.Namespace != "":
		rw.logger.Info("Namespace filter has been enabled. Nodes and namespaces will not be observed.", zap.String("namespace", rw.config.Namespace))
		factories[rw.config.Namespace] = informers.NewSharedInformerFactoryWithOptions(
			rw.client,
			rw.config.MetadataCollectionInterval,
			informers.WithNamespace(rw.config.Namespace),
		)
	default:
		// if no namespace is provided, the informer observes the whole cluster, and is stored under
		// the key "<cluster-wide-informer-key>"
		factories[metadata.ClusterWideInformerKey] = informers.NewSharedInformerFactoryWithOptions(
			rw.client,
			rw.config.MetadataCollectionInterval,
		)
	}

	return factories
}

func (rw *resourceWatcher) isKindSupported(gvk schema.GroupVersionKind) (bool, error) {
	resources, err := rw.client.Discovery().ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) { // if the discovery endpoint isn't present, assume group version is not supported
			rw.logger.Debug("Group version is not supported", zap.String("group", gvk.GroupVersion().String()))
			return false, nil
		}
		return false, fmt.Errorf("failed to fetch group version details: %w", err)
	}

	for i := range resources.APIResources {
		r := &resources.APIResources[i]
		if r.Kind == gvk.Kind {
			return true, nil
		}
	}
	return false, nil
}

// setupInformerForKind creates the informers for the given GVKs, based on the provided informer factories.
// The factories are provided as a map[string]informers.SharedInformerFactory where the map keys represent the namespace
// of the informer factory. For cluster wide informers, an empty string is used as a key
func (rw *resourceWatcher) setupInformerForKind(kind schema.GroupVersionKind, factories map[string]informers.SharedInformerFactory) {
	switch kind {
	case gvk.Pod:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Core().V1().Pods().Informer())
		}
	case gvk.Node:
		if len(rw.config.Namespaces) == 0 && rw.config.Namespace == "" && len(factories) >= 1 {
			// if no namespace is provided, the cluster wide informer factory, which is stored under the key "" is used to create the informer
			if factory, ok := factories[metadata.ClusterWideInformerKey]; ok {
				rw.setupInformer(kind, metadata.ClusterWideInformerKey, factory.Core().V1().Nodes().Informer())
			}
		}
	case gvk.Namespace:
		if len(rw.config.Namespaces) == 0 && rw.config.Namespace == "" && len(factories) >= 1 {
			// if no namespace is provided, the cluster wide informer factory, which is stored under the key "" is used to create the informer
			if factory, ok := factories[metadata.ClusterWideInformerKey]; ok {
				rw.setupInformer(kind, metadata.ClusterWideInformerKey, factory.Core().V1().Namespaces().Informer())
			}
		}
	case gvk.ReplicationController:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Core().V1().ReplicationControllers().Informer())
		}
	case gvk.ResourceQuota:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Core().V1().ResourceQuotas().Informer())
		}
	case gvk.Service:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Core().V1().Services().Informer())
		}
	case gvk.EndpointSlice:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Discovery().V1().EndpointSlices().Informer())
		}
	case gvk.PersistentVolume:
		// PersistentVolumes are cluster-scoped, so only use cluster-wide informer
		if len(rw.config.Namespaces) == 0 && rw.config.Namespace == "" && len(factories) >= 1 {
			if factory, ok := factories[metadata.ClusterWideInformerKey]; ok {
				rw.setupInformer(kind, metadata.ClusterWideInformerKey, factory.Core().V1().PersistentVolumes().Informer())
			}
		}
	case gvk.PersistentVolumeClaim:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Core().V1().PersistentVolumeClaims().Informer())
		}
	case gvk.DaemonSet:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Apps().V1().DaemonSets().Informer())
		}
	case gvk.Deployment:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Apps().V1().Deployments().Informer())
		}
	case gvk.ReplicaSet:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Apps().V1().ReplicaSets().Informer())
		}
	case gvk.StatefulSet:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Apps().V1().StatefulSets().Informer())
		}
	case gvk.Job:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Batch().V1().Jobs().Informer())
		}
	case gvk.CronJob:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Batch().V1().CronJobs().Informer())
		}
	case gvk.HorizontalPodAutoscaler:
		for ns, factory := range factories {
			rw.setupInformer(kind, ns, factory.Autoscaling().V2().HorizontalPodAutoscalers().Informer())
		}
	default:
		rw.logger.Error("Could not setup an informer for provided group version kind",
			zap.String("group version kind", kind.String()))
	}
}

// startWatchingResources starts up all informers.
func (rw *resourceWatcher) startWatchingResources(ctx context.Context, inf sharedInformer) context.Context {
	var cancel context.CancelFunc
	timedContextForInitialSync, cancel := context.WithTimeout(ctx, rw.initialTimeout)

	// Start off individual informers in the factory.
	inf.Start(ctx.Done())

	// Ensure cache is synced with initial state, once informers are started up.
	// Note that the event handler can start receiving events as soon as the informers
	// are started. So it's required to ensure that the receiver does not start
	// collecting data before the cache sync since all data may not be available.
	// This method will block either till the timeout set on the context, until
	// the initial sync is complete or the parent context is cancelled.
	inf.WaitForCacheSync(timedContextForInitialSync.Done())
	defer cancel()
	return timedContextForInitialSync
}

// setupInformer adds event handlers to informers and setups a metadataStore.
func (rw *resourceWatcher) setupInformer(gvk schema.GroupVersionKind, namespace string, informer cache.SharedIndexInformer) {
	err := informer.SetTransform(transformObject)
	if err != nil {
		rw.logger.Error("error setting informer transform function", zap.Error(err))
	}
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    rw.onAdd,
		UpdateFunc: rw.onUpdate,
		DeleteFunc: rw.onDelete,
	})
	if err != nil {
		rw.logger.Error("error adding event handler to informer", zap.Error(err))
	}
	rw.metadataStore.Setup(gvk, namespace, informer.GetStore())
}

func (rw *resourceWatcher) onAdd(obj any) {
	rw.waitForInitialInformerSync()

	// Sync metadata only if there's at least one destination for it to sent.
	if !rw.hasDestination() {
		return
	}

	rw.syncMetadataUpdate(map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata{}, rw.objMetadata(obj))
}

func (rw *resourceWatcher) hasDestination() bool {
	return len(rw.metadataConsumers) != 0 || rw.entityLogConsumer != nil
}

func (rw *resourceWatcher) onUpdate(oldObj, newObj any) {
	rw.waitForInitialInformerSync()

	// Sync metadata only if there's at least one destination for it to sent.
	if !rw.hasDestination() {
		return
	}

	rw.syncMetadataUpdate(rw.objMetadata(oldObj), rw.objMetadata(newObj))
}

func (rw *resourceWatcher) onDelete(oldObj any) {
	rw.waitForInitialInformerSync()

	// Sync metadata only if there's at least one destination for it to sent.
	if !rw.hasDestination() {
		return
	}

	rw.syncMetadataUpdate(rw.objMetadata(oldObj), map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata{})
}

// objMetadata returns the metadata for the given object.
func (rw *resourceWatcher) objMetadata(obj any) map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata {
	switch o := obj.(type) {
	case *corev1.Pod:
		return pod.GetMetadata(o, rw.metadataStore, rw.logger)
	case *corev1.Node:
		return node.GetMetadata(o)
	case *corev1.ReplicationController:
		return replicationcontroller.GetMetadata(o)
	case *corev1.Service:
		return service.GetMetadata(o)
	case *appsv1.Deployment:
		return deployment.GetMetadata(o)
	case *appsv1.ReplicaSet:
		return replicaset.GetMetadata(o)
	case *appsv1.DaemonSet:
		return daemonset.GetMetadata(o)
	case *appsv1.StatefulSet:
		return statefulset.GetMetadata(o)
	case *batchv1.Job:
		return jobs.GetMetadata(o)
	case *batchv1.CronJob:
		return cronjob.GetMetadata(o)
	case *autoscalingv2.HorizontalPodAutoscaler:
		return hpa.GetMetadata(o)
	case *corev1.Namespace:
		return namespace.GetMetadata(o)
	case *corev1.PersistentVolume:
		return persistentvolume.GetMetadata(o)
	case *corev1.PersistentVolumeClaim:
		return persistentvolumeclaim.GetMetadata(o)
	}
	return nil
}

func (rw *resourceWatcher) waitForInitialInformerSync() {
	if rw.initialSyncDone.Load() || rw.initialSyncTimedOut.Load() {
		return
	}

	// Wait till initial sync is complete or timeout.
	for !rw.initialSyncDone.Load() {
		if rw.initialSyncTimedOut.Load() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (rw *resourceWatcher) setupMetadataExporters(
	exporters map[component.ID]component.Component,
	metadataExportersFromConfig []string,
) error {
	var out []metadataConsumer

	metadataExportersSet := stringSliceToMap(metadataExportersFromConfig)
	if err := validateMetadataExporters(metadataExportersSet, exporters); err != nil {
		return fmt.Errorf("failed to configure metadata_exporters: %w", err)
	}

	for cfg, exp := range exporters {
		if !metadataExportersSet[cfg.String()] {
			continue
		}
		kme, ok := exp.(experimentalmetricmetadata.MetadataExporter)
		if !ok {
			return fmt.Errorf("%s exporter does not implement MetadataExporter", cfg.Name())
		}
		out = append(out, kme.ConsumeMetadata)
		rw.logger.Info("Configured Kubernetes MetadataExporter",
			zap.String("exporter_name", cfg.String()),
		)
	}

	rw.metadataConsumers = out
	return nil
}

func validateMetadataExporters(metadataExporters map[string]bool, exporters map[component.ID]component.Component) error {
	configuredExporters := map[string]bool{}
	for cfg := range exporters {
		configuredExporters[cfg.String()] = true
	}

	for e := range metadataExporters {
		if !configuredExporters[e] {
			return fmt.Errorf("%s exporter is not in collector config", e)
		}
	}

	return nil
}

func (rw *resourceWatcher) syncMetadataUpdate(oldMetadata, newMetadata map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata) {
	timestamp := pcommon.NewTimestampFromTime(time.Now())

	metadataUpdate := metadata.GetMetadataUpdate(oldMetadata, newMetadata)
	if len(metadataUpdate) != 0 {
		for _, consume := range rw.metadataConsumers {
			_ = consume(metadataUpdate)
		}
	}

	if rw.entityLogConsumer != nil {
		// Represent metadata update as entity events.
		entityEvents := metadata.GetEntityEvents(oldMetadata, newMetadata, timestamp, rw.config.MetadataCollectionInterval)

		// Convert entity events to log representation.
		logs := entityEvents.ConvertAndMoveToLogs()

		if logs.LogRecordCount() != 0 {
			err := rw.entityLogConsumer.ConsumeLogs(context.Background(), logs)
			if err != nil {
				rw.logger.Error("Error sending entity events to the consumer", zap.Error(err))

				// Note: receiver contract says that we need to retry sending if the
				// returned error is not Permanent. However, we are not doing it here.
				// Instead, we rely on the fact the metadata is collected periodically
				// and the entity events will be delivered on the next cycle. This is
				// fine because we deliver cumulative entity state.
				// This allows us to avoid stressing the Collector or its destination
				// unnecessarily (typically non-Permanent errors happen in stressed conditions).
				// The periodic collection will be implemented later, see
				// https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/24413
			}
		}
	}
}

// stringSliceToMap converts a slice of strings into a map with keys from the slice
func stringSliceToMap(strings []string) map[string]bool {
	ret := map[string]bool{}
	for _, s := range strings {
		ret[s] = true
	}
	return ret
}
