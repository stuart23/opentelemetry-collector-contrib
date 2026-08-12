// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package metadata // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/k8sclusterreceiver/internal/metadata"

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CustomResourceConfig configures an additional, explicitly-named Kubernetes
// custom resource (CRD-backed type) to watch, in addition to the receiver's
// built-in supported kinds.
type CustomResourceConfig struct {
	// Group is the API group of the custom resource, e.g. "helm.toolkit.fluxcd.io".
	Group string `mapstructure:"group"`
	// Version is the API version of the custom resource, e.g. "v2". If unset, the
	// server's preferred version for this Group/Resource is resolved via discovery
	// at startup. Only the preferred version is watched even if a CRD serves
	// several - every served version of a CRD is a view onto the same stored
	// objects, so watching more than one would double-count the same resources.
	Version string `mapstructure:"version"`
	// Resource is the plural resource name of the custom resource, e.g. "helmreleases".
	Resource string `mapstructure:"resource"`
	// PhaseField is the dot-separated path within the object to read its phase/state
	// string from. Defaults to "status.phase" if unset.
	PhaseField string `mapstructure:"phase_field"`
	// PhaseMapping adds to, or overrides, the default phase string to value mapping
	// for this resource.
	PhaseMapping map[string]int64 `mapstructure:"phase_mapping"`
}

// GroupVersionResource returns the schema.GroupVersionResource this config identifies.
func (c CustomResourceConfig) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: c.Group, Version: c.Version, Resource: c.Resource}
}

// GroupVersionKind returns a synthetic schema.GroupVersionKind used purely as an
// internal Store key for this configured resource. Custom resource objects are
// represented as *unstructured.Unstructured and carry their own real Kind, so this
// exists only to give each configured resource a stable, unique bookkeeping key
// that's derivable from config alone, using the plural resource name in place of a
// Kind. It is never surfaced in emitted telemetry - the real Kind (read off each
// object) is used for that.
func (c CustomResourceConfig) GroupVersionKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: c.Group, Version: c.Version, Kind: c.Resource}
}

// PhaseFieldPath returns the configured PhaseField split into its path segments,
// defaulting to []string{"status", "phase"}.
func (c CustomResourceConfig) PhaseFieldPath() []string {
	if c.PhaseField == "" {
		return []string{"status", "phase"}
	}
	return strings.Split(c.PhaseField, ".")
}

// ResolvePhase maps a raw phase string to its numeric value, checking this
// resource's PhaseMapping first, then DefaultPhaseMapping, and finally falling
// back to UnmappedPhaseValue.
func (c CustomResourceConfig) ResolvePhase(raw string) int64 {
	if v, ok := c.PhaseMapping[raw]; ok {
		return v
	}
	if v, ok := DefaultPhaseMapping[raw]; ok {
		return v
	}
	return UnmappedPhaseValue
}

// DefaultPhaseMapping is the built-in phase-string to value mapping applied to all
// configured custom resources, before any per-resource CustomResourceConfig.PhaseMapping
// overrides/additions.
var DefaultPhaseMapping = map[string]int64{
	"Pending":      1,
	"New":          1,
	"Provisioning": 1,
	"Progressing":  1,
	"Ready":        2,
	"Active":       2,
	"Running":      2,
	"Terminating":  3,
	"Deleting":     3,
	"Succeeded":    3,
	"Failed":       4,
	"Error":        4,
}

// UnmappedPhaseValue is used for phase strings (including empty/missing values)
// that don't match any entry in the default or per-resource mapping.
const UnmappedPhaseValue int64 = 5
