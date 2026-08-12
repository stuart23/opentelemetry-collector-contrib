// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCustomResourceConfigGroupVersionResource(t *testing.T) {
	cfg := CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	assert.Equal(t,
		schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
		cfg.GroupVersionResource())
}

func TestCustomResourceConfigGroupVersionKind(t *testing.T) {
	cfg := CustomResourceConfig{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	assert.Equal(t,
		schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "helmreleases"},
		cfg.GroupVersionKind())
}

func TestCustomResourceConfigPhaseFieldPath(t *testing.T) {
	tests := []struct {
		name     string
		cfg      CustomResourceConfig
		expected []string
	}{
		{
			name:     "default",
			cfg:      CustomResourceConfig{},
			expected: []string{"status", "phase"},
		},
		{
			name:     "custom nested path",
			cfg:      CustomResourceConfig{PhaseField: "status.conditions.state"},
			expected: []string{"status", "conditions", "state"},
		},
		{
			name:     "custom top-level path",
			cfg:      CustomResourceConfig{PhaseField: "state"},
			expected: []string{"state"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.cfg.PhaseFieldPath())
		})
	}
}

func TestCustomResourceConfigResolvePhase(t *testing.T) {
	tests := []struct {
		name     string
		cfg      CustomResourceConfig
		raw      string
		expected int64
	}{
		{name: "default pending-like", cfg: CustomResourceConfig{}, raw: "Pending", expected: 1},
		{name: "default new", cfg: CustomResourceConfig{}, raw: "New", expected: 1},
		{name: "default ready-like", cfg: CustomResourceConfig{}, raw: "Ready", expected: 2},
		{name: "default active", cfg: CustomResourceConfig{}, raw: "Active", expected: 2},
		{name: "default terminating-like", cfg: CustomResourceConfig{}, raw: "Terminating", expected: 3},
		{name: "default succeeded", cfg: CustomResourceConfig{}, raw: "Succeeded", expected: 3},
		{name: "default failed-like", cfg: CustomResourceConfig{}, raw: "Failed", expected: 4},
		{name: "default error", cfg: CustomResourceConfig{}, raw: "Error", expected: 4},
		{name: "unmapped", cfg: CustomResourceConfig{}, raw: "SomethingElse", expected: UnmappedPhaseValue},
		{name: "empty/missing", cfg: CustomResourceConfig{}, raw: "", expected: UnmappedPhaseValue},
		{
			name:     "per-resource addition",
			cfg:      CustomResourceConfig{PhaseMapping: map[string]int64{"WaitingForRetentionExpiryToDelete": 99}},
			raw:      "WaitingForRetentionExpiryToDelete",
			expected: 99,
		},
		{
			name:     "per-resource override of a default entry",
			cfg:      CustomResourceConfig{PhaseMapping: map[string]int64{"Ready": 42}},
			raw:      "Ready",
			expected: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.cfg.ResolvePhase(tt.raw))
		})
	}
}
