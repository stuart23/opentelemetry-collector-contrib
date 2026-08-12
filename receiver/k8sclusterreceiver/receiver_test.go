// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package k8sclusterreceiver

import (
	"context"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	quotaclientset "github.com/openshift/client-go/quota/clientset/versioned"
	fakeQuota "github.com/openshift/client-go/quota/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/receiver"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/k8sconfig"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/k8sleaderelectortest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/gvk"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/metadata"
)

func init() {
	// Disable WatchListClient feature gate for tests as fake clientset doesn't support bookmark events
	// See: https://github.com/kubernetes/kubernetes/issues/129408
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
}

type nopHost struct {
	component.Host
}

func (*nopHost) GetExporters() map[pipeline.Signal]map[component.ID]component.Component {
	return nil
}

func newNopHost() component.Host {
	return &nopHost{
		componenttest.NewNopHost(),
	}
}

func TestReceiver(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()
	osQuotaClient := fakeQuota.NewClientset()
	sink := new(consumertest.MetricsSink)

	r := setupReceiver(client, osQuotaClient, sink, nil, 10*time.Second, tt, nil, component.MustNewID("foo"))

	// Setup k8s resources.
	numPods := 2
	numNodes := 1
	numQuotas := 2
	numClusterQuotaMetrics := numQuotas * 4
	createPods(t, client, numPods, false)
	createNodes(t, client, numNodes)
	createClusterQuota(t, osQuotaClient, 2)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHost()))

	// Expects metric data from nodes and pods where each metric data
	// struct corresponds to one resource.
	expectedNumMetrics := numPods + numNodes + numClusterQuotaMetrics
	var initialDataPointCount int
	require.Eventually(t, func() bool {
		initialDataPointCount = sink.DataPointCount()
		return initialDataPointCount == expectedNumMetrics
	}, 10*time.Second, 100*time.Millisecond,
		"metrics not collected")

	numPodsToDelete := 1
	deletePods(t, client, numPodsToDelete)

	// Expects metric data from a node, since other resources were deleted.
	expectedNumMetrics = (numPods - numPodsToDelete) + numNodes + numClusterQuotaMetrics
	var metricsCountDelta int
	require.Eventually(t, func() bool {
		metricsCountDelta = sink.DataPointCount() - initialDataPointCount
		return metricsCountDelta == expectedNumMetrics
	}, 10*time.Second, 100*time.Millisecond,
		"updated metrics not collected")

	require.NoError(t, r.Shutdown(ctx))
}

func TestReceiverWithLeaderElection(t *testing.T) {
	fakeLeaderElection := &k8sleaderelectortest.FakeLeaderElection{}
	fakeHost := &k8sleaderelectortest.FakeHost{
		FakeLeaderElection: fakeLeaderElection,
	}

	client := newFakeClientWithAllResources()
	osQuotaClient := fakeQuota.NewClientset()
	sink := new(consumertest.MetricsSink)
	tt := componenttest.NewTelemetry()
	kr := setupReceiver(client, osQuotaClient, sink, nil, 10*time.Second, tt, nil, component.MustNewID("k8s_leader_elector"))

	// Setup k8s resources.
	numPods := 2

	createPods(t, client, numPods, false)

	err := kr.Start(t.Context(), fakeHost)
	require.NoError(t, err)

	// elected leader
	fakeLeaderElection.InvokeOnLeading()

	expectedNumMetrics := numPods
	var initialDataPointCount int
	require.Eventually(t, func() bool {
		initialDataPointCount = sink.DataPointCount()
		return initialDataPointCount == expectedNumMetrics
	}, 20*time.Second, 100*time.Millisecond,
		"metrics not collected")

	// lost election
	fakeLeaderElection.InvokeOnStopping()
	require.NoError(t, kr.Shutdown(t.Context()))
}

func TestNamespacedReceiver(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()
	osQuotaClient := fakeQuota.NewClientset()
	sink := new(consumertest.MetricsSink)

	r := setupReceiver(client, osQuotaClient, sink, nil, 10*time.Second, tt, []string{"test"}, component.MustNewID("foo"))

	// Setup k8s resources.
	numPods := 2
	numNodes := 1
	numQuotas := 2

	createPods(t, client, numPods, false)
	createNodes(t, client, numNodes)
	createClusterQuota(t, osQuotaClient, numQuotas)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHost()))

	// Expects metric data from pods  only, where each metric data
	// struct corresponds to one resource.
	// Nodes and ClusterResourceQuotas should not be observed as these are non-namespaced resources
	expectedNumMetrics := numPods
	var initialDataPointCount int
	require.Eventually(t, func() bool {
		initialDataPointCount = sink.DataPointCount()
		return initialDataPointCount == expectedNumMetrics
	}, 10*time.Second, 100*time.Millisecond,
		"metrics not collected")

	numPodsToDelete := 1
	deletePods(t, client, numPodsToDelete)

	// Expects metric data from a node, since other resources were deleted.
	expectedNumMetrics = numPods - numPodsToDelete
	var metricsCountDelta int
	require.Eventually(t, func() bool {
		metricsCountDelta = sink.DataPointCount() - initialDataPointCount
		return metricsCountDelta == expectedNumMetrics
	}, 10*time.Second, 100*time.Millisecond,
		"updated metrics not collected")

	require.NoError(t, r.Shutdown(ctx))
}

func TestNamespacedReceiverWithMultipleNamespaces(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()
	osQuotaClient := fakeQuota.NewClientset()
	sink := new(consumertest.MetricsSink)

	observedNamespaces := []string{"test-0", "test-1"}
	r := setupReceiver(client, osQuotaClient, sink, nil, 10*time.Second, tt, observedNamespaces, component.MustNewID("foo"))

	// Setup k8s resources.
	numPods := 3
	numNodes := 1
	numQuotas := 1

	createPods(t, client, numPods, true)
	createNodes(t, client, numNodes)
	createClusterQuota(t, osQuotaClient, numQuotas)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHost()))

	require.Eventually(t, func() bool {
		m := sink.AllMetrics()

		if len(m) == 0 {
			return false
		}
		gotNamespaces := []string{}
		latestMetrics := m[len(m)-1].ResourceMetrics()

		for i := 0; i < latestMetrics.Len(); i++ {
			resource := latestMetrics.At(i).Resource()
			gotNamespace, ok := resource.Attributes().Get("k8s.namespace.name")
			if ok {
				gotNamespaces = append(gotNamespaces, gotNamespace.AsString())
			}
		}
		slices.Sort(gotNamespaces)

		return slices.Equal(observedNamespaces, gotNamespaces)
	}, 10*time.Second, 100*time.Millisecond,
		"metrics not collected")

	require.NoError(t, r.Shutdown(ctx))
}

func TestReceiverTimesOutAfterStartup(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()
	client := newFakeClientWithAllResources()

	// Mock initial cache sync timing out, using a small timeout.
	r := setupReceiver(client, nil, consumertest.NewNop(), nil, 1*time.Millisecond, tt, nil, component.MustNewID("foo"))

	createPods(t, client, 1, false)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHost()))
	require.Eventually(t, func() bool {
		return r.resourceWatcher.initialSyncTimedOut.Load()
	}, 10*time.Second, 100*time.Millisecond)
	require.NoError(t, r.Shutdown(ctx))
}

func TestReceiverWithManyResources(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()
	osQuotaClient := fakeQuota.NewClientset()
	sink := new(consumertest.MetricsSink)

	r := setupReceiver(client, osQuotaClient, sink, nil, 10*time.Second, tt, nil, component.MustNewID("foo"))

	numPods := 1000
	numQuotas := 2
	numExpectedMetrics := numPods + numQuotas*4
	createPods(t, client, numPods, false)
	createClusterQuota(t, osQuotaClient, 2)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHost()))

	require.Eventually(t, func() bool {
		// 4 points from the cluster quota.
		return sink.DataPointCount() == numExpectedMetrics
	}, 10*time.Second, 100*time.Millisecond,
		"metrics not collected")

	require.NoError(t, r.Shutdown(ctx))
}

var (
	numCalls                  *atomic.Int32
	consumeMetadataInvocation = func() {
		if numCalls != nil {
			numCalls.Add(1)
		}
	}
)

func TestReceiverWithMetadata(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()
	metricsConsumer := &mockExporterWithK8sMetadata{MetricsSink: new(consumertest.MetricsSink)}
	numCalls = &atomic.Int32{}

	logsConsumer := new(consumertest.LogsSink)

	r := setupReceiver(client, nil, metricsConsumer, logsConsumer, 10*time.Second, tt, nil, component.MustNewID("foo"))
	r.config.MetadataExporters = []string{"nop/withmetadata"}

	// Setup k8s resources.
	pods := createPods(t, client, 1, false)

	ctx := t.Context()
	require.NoError(t, r.Start(ctx, newNopHostWithExporters()))

	// Mock an update on the Pod object. It appears that the fake clientset
	// does not pass on events for updates to resources.
	require.Len(t, pods, 1)
	updatedPod := getUpdatedPod(pods[0])
	r.resourceWatcher.onUpdate(pods[0], updatedPod)

	// Should not result in ConsumerKubernetesMetadata invocation since the pod
	// is not changed. Should result in entity event because they are emitted even
	// if the entity is not changed.
	r.resourceWatcher.onUpdate(updatedPod, updatedPod)

	deletePods(t, client, 1)

	// Ensure ConsumeKubernetesMetadata is called twice, once for the add and
	// then for the update. Note the second update does not result in metadata call
	// since the pod is not changed.
	require.Eventually(t, func() bool {
		return int(numCalls.Load()) == 2
	}, 10*time.Second, 100*time.Millisecond,
		"metadata not collected")

	// Must have 4 entity events: once for the add, followed by an update and
	// then another update, and then a delete.
	require.Eventually(t, func() bool {
		return logsConsumer.LogRecordCount() == 4
	}, 10*time.Second, 100*time.Millisecond,
		"entity events not collected")

	require.NoError(t, r.Shutdown(ctx))
}

// TestReceiverWithCustomResources exercises the full custom-resource path end to
// end: dynamic informer creation and startup, the create/update/delete event
// handlers registered in setupCustomResourceInformer, entity event emission, and
// k8s.customresource.phase metric collection - through the real receiver Start
// flow, not by calling the internal helpers directly.
func TestReceiverWithCustomResources(t *testing.T) {
	tt := componenttest.NewTelemetry()
	defer func() {
		require.NoError(t, tt.Shutdown(t.Context()))
	}()

	client := newFakeClientWithAllResources()

	gvr := schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	newHelmRelease := func(phase string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "helm.toolkit.fluxcd.io/v2",
				"kind":       "HelmRelease",
				"metadata": map[string]any{
					"name":      "release1",
					"namespace": "test-namespace",
					"uid":       "release1-uid",
				},
				"status": map[string]any{"phase": phase},
			},
		}
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "HelmReleaseList"},
		newHelmRelease("Ready"),
	)

	config := &Config{
		CollectionInterval:   200 * time.Millisecond,
		Distribution:         distributionKubernetes,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		Resources: []metadata.CustomResourceConfig{
			{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource},
		},
	}

	metricsSink := new(consumertest.MetricsSink)
	logsSink := new(consumertest.LogsSink)

	r, err := newReceiver(t.Context(), receiver.Settings{ID: component.NewID(metadata.Type), TelemetrySettings: tt.NewTelemetrySettings(), BuildInfo: component.NewDefaultBuildInfo()}, config)
	require.NoError(t, err)
	kr := r.(*kubernetesReceiver)
	kr.metricsConsumer = metricsSink
	kr.resourceWatcher.entityLogConsumer = logsSink
	kr.resourceWatcher.makeClient = func(_ k8sconfig.APIConfig) (kubernetes.Interface, error) {
		return client, nil
	}
	kr.resourceWatcher.makeDynamicClient = func(_ k8sconfig.APIConfig) (dynamic.Interface, error) {
		return dynClient, nil
	}
	kr.resourceWatcher.initialTimeout = 10 * time.Second

	ctx := t.Context()
	require.NoError(t, kr.Start(ctx, newNopHost()))

	findPhaseDataPoint := func() (int64, bool) {
		for _, m := range metricsSink.AllMetrics() {
			rms := m.ResourceMetrics()
			for i := 0; i < rms.Len(); i++ {
				sms := rms.At(i).ScopeMetrics()
				for j := 0; j < sms.Len(); j++ {
					ms := sms.At(j).Metrics()
					for k := 0; k < ms.Len(); k++ {
						if ms.At(k).Name() == "k8s.customresource.phase" {
							dps := ms.At(k).Gauge().DataPoints()
							if dps.Len() > 0 {
								return dps.At(dps.Len() - 1).IntValue(), true
							}
						}
					}
				}
			}
		}
		return 0, false
	}

	// The initial object (phase: Ready) should be collected as phase value 2.
	require.Eventually(t, func() bool {
		v, ok := findPhaseDataPoint()
		return ok && v == 2
	}, 10*time.Second, 100*time.Millisecond, "initial k8s.customresource.phase metric not collected")

	// The create should also have produced an entity event for the k8s.helmrelease entity.
	require.Eventually(t, func() bool {
		for _, l := range logsSink.AllLogs() {
			rls := l.ResourceLogs()
			for i := 0; i < rls.Len(); i++ {
				sls := rls.At(i).ScopeLogs()
				for j := 0; j < sls.Len(); j++ {
					lrs := sls.At(j).LogRecords()
					for k := 0; k < lrs.Len(); k++ {
						entityType, ok := lrs.At(k).Attributes().Get("otel.entity.type")
						if ok && entityType.Str() == "k8s.helmrelease" {
							return true
						}
					}
				}
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "entity event for custom resource not emitted")

	metricsSink.Reset()

	// Update the object's phase and expect the metric to converge to the new value.
	_, err = dynClient.Resource(gvr).Namespace("test-namespace").Update(ctx, newHelmRelease("Failed"), v1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		v, ok := findPhaseDataPoint()
		return ok && v == 4
	}, 10*time.Second, 100*time.Millisecond, "updated k8s.customresource.phase metric not collected")

	// Delete the object and expect it to stop being reported.
	require.NoError(t, dynClient.Resource(gvr).Namespace("test-namespace").Delete(ctx, "release1", v1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		for _, l := range logsSink.AllLogs() {
			rls := l.ResourceLogs()
			for i := 0; i < rls.Len(); i++ {
				sls := rls.At(i).ScopeLogs()
				for j := 0; j < sls.Len(); j++ {
					lrs := sls.At(j).LogRecords()
					for k := 0; k < lrs.Len(); k++ {
						eventType, ok := lrs.At(k).Attributes().Get("otel.entity.event.type")
						if ok && eventType.Str() == "entity_delete" {
							return true
						}
					}
				}
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "entity delete event for custom resource not emitted")

	require.NoError(t, kr.Shutdown(ctx))
}

func getUpdatedPod(pod *corev1.Pod) any {
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
			Labels: map[string]string{
				"key": "value",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
		},
	}
}

func setupReceiver(client *fake.Clientset, osQuotaClient quotaclientset.Interface, metricsConsumer consumer.Metrics, logsConsumer consumer.Logs, initialSyncTimeout time.Duration, tt *componenttest.Telemetry, namespaces []string, leaderElector component.ID) *kubernetesReceiver {
	distribution := distributionKubernetes
	if osQuotaClient != nil {
		distribution = distributionOpenShift
	}
	config := &Config{
		CollectionInterval:         1 * time.Second,
		NodeConditionTypesToReport: []string{"Ready"},
		AllocatableTypesToReport:   []string{"cpu", "memory"},
		Distribution:               distribution,
		MetricsBuilderConfig:       metadata.NewDefaultMetricsBuilderConfig(),
	}

	config.Namespaces = namespaces

	if leaderElector.Type().String() == "k8s_leader_elector" {
		config.K8sLeaderElector = &leaderElector
	}

	r, _ := newReceiver(context.Background(), receiver.Settings{ID: component.NewID(metadata.Type), TelemetrySettings: tt.NewTelemetrySettings(), BuildInfo: component.NewDefaultBuildInfo()}, config)
	kr := r.(*kubernetesReceiver)
	kr.metricsConsumer = metricsConsumer
	kr.resourceWatcher.makeClient = func(_ k8sconfig.APIConfig) (kubernetes.Interface, error) {
		return client, nil
	}
	kr.resourceWatcher.makeOpenShiftQuotaClient = func(_ k8sconfig.APIConfig) (quotaclientset.Interface, error) {
		return osQuotaClient, nil
	}
	kr.resourceWatcher.initialTimeout = initialSyncTimeout
	kr.resourceWatcher.entityLogConsumer = logsConsumer
	return kr
}

func newFakeClientWithAllResources() *fake.Clientset {
	client := fake.NewClientset()
	client.Resources = []*v1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []v1.APIResource{
				gvkToAPIResource(gvk.Pod),
				gvkToAPIResource(gvk.Node),
				gvkToAPIResource(gvk.Namespace),
				gvkToAPIResource(gvk.ReplicationController),
				gvkToAPIResource(gvk.ResourceQuota),
				gvkToAPIResource(gvk.Service),
				gvkToAPIResource(gvk.PersistentVolume),
				gvkToAPIResource(gvk.PersistentVolumeClaim),
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []v1.APIResource{
				gvkToAPIResource(gvk.DaemonSet),
				gvkToAPIResource(gvk.Deployment),
				gvkToAPIResource(gvk.ReplicaSet),
				gvkToAPIResource(gvk.StatefulSet),
			},
		},
		{
			GroupVersion: "batch/v1",
			APIResources: []v1.APIResource{
				gvkToAPIResource(gvk.Job),
				gvkToAPIResource(gvk.CronJob),
			},
		},
		{
			GroupVersion: "autoscaling/v2",
			APIResources: []v1.APIResource{
				gvkToAPIResource(gvk.HorizontalPodAutoscaler),
			},
		},
		{
			GroupVersion: "discovery.k8s.io/v1",
			APIResources: []v1.APIResource{
				gvkToAPIResource(gvk.EndpointSlice),
			},
		},
	}
	return client
}

func gvkToAPIResource(gvk schema.GroupVersionKind) v1.APIResource {
	return v1.APIResource{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind,
	}
}
