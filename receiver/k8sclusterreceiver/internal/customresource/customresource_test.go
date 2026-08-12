// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package customresource

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/metadata"
)

func newHelmRelease(phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2",
			"kind":       "HelmRelease",
			"metadata": map[string]any{
				"name":      "test-release",
				"namespace": "test-namespace",
				"uid":       "test-release-uid",
				"labels": map[string]any{
					"app": "test",
				},
			},
		},
	}
	if phase != "" {
		obj.Object["status"] = map[string]any{"phase": phase}
	}
	return obj
}

func TestGetMetadata(t *testing.T) {
	cfg := metadata.CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	obj := newHelmRelease("Ready")

	got := GetMetadata(obj, cfg)

	require.Len(t, got, 1)
	km, ok := got[experimentalmetricmetadata.ResourceID("test-release-uid")]
	require.True(t, ok)

	assert.Equal(t, "k8s.helmrelease", km.EntityType)
	assert.Equal(t, "k8s.helmrelease.uid", km.ResourceIDKey)
	assert.Equal(t, "Ready", km.Metadata["k8s.customresource.phase"])
	assert.Equal(t, "test", km.Metadata["k8s.helmrelease.label.app"])
	assert.Equal(t, "test-namespace", km.Metadata["k8s.namespace.name"])
}

func TestGetMetadataMissingPhase(t *testing.T) {
	cfg := metadata.CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	obj := newHelmRelease("")

	got := GetMetadata(obj, cfg)

	km := got[experimentalmetricmetadata.ResourceID("test-release-uid")]
	_, ok := km.Metadata["k8s.customresource.phase"]
	assert.False(t, ok, "phase metadata should not be set when the configured phase_field is absent")
}

func TestRecordMetrics(t *testing.T) {
	cfg := metadata.CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	obj := newHelmRelease("Ready")

	ts := pcommon.Timestamp(time.Now().UnixNano())
	mb := metadata.NewMetricsBuilder(metadata.NewDefaultMetricsBuilderConfig(), receivertest.NewNopSettings(metadata.Type))
	RecordMetrics(mb, cfg, obj, ts)
	m := mb.Emit()

	require.Equal(t, 1, m.ResourceMetrics().Len())
	rm := m.ResourceMetrics().At(0)
	assert.Equal(t,
		map[string]any{
			"k8s.customresource.uid":     "test-release-uid",
			"k8s.customresource.name":    "test-release",
			"k8s.namespace.name":         "test-namespace",
			"k8s.customresource.kind":    "HelmRelease",
			"k8s.customresource.group":   "helm.toolkit.fluxcd.io",
			"k8s.customresource.version": "v2",
		},
		rm.Resource().Attributes().AsRaw())

	require.Equal(t, 1, rm.ScopeMetrics().Len())
	sms := rm.ScopeMetrics().At(0)
	require.Equal(t, 1, sms.Metrics().Len())
	metric := sms.Metrics().At(0)
	assert.Equal(t, "k8s.customresource.phase", metric.Name())
	require.Equal(t, 1, metric.Gauge().DataPoints().Len())
	assert.Equal(t, int64(2), metric.Gauge().DataPoints().At(0).IntValue())
}

func TestRecordMetricsUnmappedPhase(t *testing.T) {
	cfg := metadata.CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	obj := newHelmRelease("SomeUnknownState")

	ts := pcommon.Timestamp(time.Now().UnixNano())
	mb := metadata.NewMetricsBuilder(metadata.NewDefaultMetricsBuilderConfig(), receivertest.NewNopSettings(metadata.Type))
	RecordMetrics(mb, cfg, obj, ts)
	m := mb.Emit()

	metric := m.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, metadata.UnmappedPhaseValue, metric.Gauge().DataPoints().At(0).IntValue())
}

func TestRecordMetricsCustomPhaseMapping(t *testing.T) {
	cfg := metadata.CustomResourceConfig{
		Group:        "helm.toolkit.fluxcd.io",
		Version:      "v2",
		Resource:     "helmreleases",
		PhaseMapping: map[string]int64{"WaitingForRetentionExpiryToDelete": 99},
	}
	obj := newHelmRelease("WaitingForRetentionExpiryToDelete")

	ts := pcommon.Timestamp(time.Now().UnixNano())
	mb := metadata.NewMetricsBuilder(metadata.NewDefaultMetricsBuilderConfig(), receivertest.NewNopSettings(metadata.Type))
	RecordMetrics(mb, cfg, obj, ts)
	m := mb.Emit()

	metric := m.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, int64(99), metric.Gauge().DataPoints().At(0).IntValue())
}

func TestRecordMetricsCustomPhaseField(t *testing.T) {
	cfg := metadata.CustomResourceConfig{
		Group:      "helm.toolkit.fluxcd.io",
		Version:    "v2",
		Resource:   "helmreleases",
		PhaseField: "status.conditions.state",
	}
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2",
			"kind":       "HelmRelease",
			"metadata": map[string]any{
				"name":      "test-release",
				"namespace": "test-namespace",
				"uid":       "test-release-uid",
			},
			"status": map[string]any{
				"conditions": map[string]any{"state": "Running"},
			},
		},
	}

	ts := pcommon.Timestamp(time.Now().UnixNano())
	mb := metadata.NewMetricsBuilder(metadata.NewDefaultMetricsBuilderConfig(), receivertest.NewNopSettings(metadata.Type))
	RecordMetrics(mb, cfg, obj, ts)
	m := mb.Emit()

	metric := m.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, int64(2), metric.Gauge().DataPoints().At(0).IntValue())
}
