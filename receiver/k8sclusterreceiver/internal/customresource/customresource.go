// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package customresource provides generic metadata and metric collection for
// arbitrary custom resources (CRDs) configured via the receiver's `resources`
// option. Unlike every other internal/* package here, it isn't specific to a
// single Kubernetes kind - it operates on *unstructured.Unstructured so that
// one code path can serve any CRD-backed type the user configures.
package customresource // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/customresource"

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/metadata"
)

// rawPhaseMetadataKey is the metadata key the raw (unmapped) phase string is
// stored under, alongside the generic metadata GetGenericMetadata already
// collects (labels, owner references, etc). This preserves the original string
// value for consumers of the metadata-exporter/entity-event pipeline even
// though the k8s.customresource.phase metric itself is numeric.
const rawPhaseMetadataKey = "k8s.customresource.phase"

// GetMetadata returns generic metadata for a configured custom resource object.
func GetMetadata(obj *unstructured.Unstructured, cfg metadata.CustomResourceConfig) map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata {
	om := objectMeta(obj)
	km := metadata.GetGenericMetadata(&om, obj.GetKind())

	if raw, found, _ := unstructured.NestedString(obj.Object, cfg.PhaseFieldPath()...); found {
		km.Metadata[rawPhaseMetadataKey] = raw
	}

	return map[experimentalmetricmetadata.ResourceID]*metadata.KubernetesMetadata{
		experimentalmetricmetadata.ResourceID(om.UID): km,
	}
}

// RecordMetrics records the k8s.customresource.phase metric and emits the
// k8s.customresource entity for a configured custom resource object.
func RecordMetrics(mb *metadata.MetricsBuilder, cfg metadata.CustomResourceConfig, obj *unstructured.Unstructured, ts pcommon.Timestamp) {
	gvk := obj.GroupVersionKind()

	e := metadata.NewK8sCustomresourceEntity(string(obj.GetUID()))
	e.SetK8sCustomresourceName(obj.GetName())
	e.SetK8sNamespaceName(obj.GetNamespace())
	e.SetK8sCustomresourceKind(gvk.Kind)
	e.SetK8sCustomresourceGroup(gvk.Group)
	e.SetK8sCustomresourceVersion(gvk.Version)
	eb := mb.ForK8sCustomresource(e)

	raw, _, _ := unstructured.NestedString(obj.Object, cfg.PhaseFieldPath()...)
	eb.RecordK8sCustomresourcePhaseDataPoint(ts, cfg.ResolvePhase(raw))

	eb.Emit()
}

func objectMeta(obj *unstructured.Unstructured) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:              obj.GetName(),
		Namespace:         obj.GetNamespace(),
		UID:               obj.GetUID(),
		CreationTimestamp: obj.GetCreationTimestamp(),
		Labels:            obj.GetLabels(),
		OwnerReferences:   obj.GetOwnerReferences(),
	}
}
