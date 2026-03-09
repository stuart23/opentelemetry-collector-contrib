// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

func TestScraperStartFailure(t *testing.T) {
	cfg := &Config{Endpoint: "ws://localhost:19/invalid"}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := scraper.start(ctx, nil)
	assert.Error(t, err)
}

func TestScraperScrapeNoClient(t *testing.T) {
	cfg := &Config{Endpoint: "ws://localhost:9222"}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	metrics, err := scraper.scrape(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CDP client not initialized")
	assert.NotNil(t, metrics)
}

func TestScraperShutdownWithoutStart(t *testing.T) {
	cfg := &Config{Endpoint: "ws://localhost:9222"}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	assert.NoError(t, scraper.shutdown(ctx))
}

func TestRecordPerformanceMetrics(t *testing.T) {
	cfg := &Config{Endpoint: "ws://localhost:9222"}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	s := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	now := pcommon.NewTimestampFromTime(time.Now())

	perfResult := &PerformanceMetricsResult{
		Metrics: []PerformanceMetric{
			{Name: "Documents", Value: 3},
			{Name: "Frames", Value: 2},
			{Name: "JSEventListeners", Value: 15},
			{Name: "Nodes", Value: 150},
			{Name: "LayoutCount", Value: 42},
			{Name: "RecalcStyleCount", Value: 10},
			{Name: "LayoutDuration", Value: 0.123},
			{Name: "RecalcStyleDuration", Value: 0.045},
			{Name: "ScriptDuration", Value: 1.234},
			{Name: "TaskDuration", Value: 2.567},
			{Name: "JSHeapUsedSize", Value: 8388608},
			{Name: "JSHeapTotalSize", Value: 16777216},
		},
	}

	s.recordPerformanceMetrics(now, perfResult, "https://example.com")

	metrics := s.mb.Emit()
	require.Greater(t, metrics.ResourceMetrics().Len(), 0)

	rm := metrics.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics().At(0)

	metricNames := make(map[string]bool)
	for i := 0; i < sm.Metrics().Len(); i++ {
		metricNames[sm.Metrics().At(i).Name()] = true
	}

	assert.True(t, metricNames["chromium.page.document.count"])
	assert.True(t, metricNames["chromium.page.frame.count"])
	assert.True(t, metricNames["chromium.page.js_event_listener.count"])
	assert.True(t, metricNames["chromium.page.dom_node.count"])
	assert.True(t, metricNames["chromium.page.layout.count"])
	assert.True(t, metricNames["chromium.page.recalc_style.count"])
	assert.True(t, metricNames["chromium.page.layout.duration"])
	assert.True(t, metricNames["chromium.page.recalc_style.duration"])
	assert.True(t, metricNames["chromium.page.script.duration"])
	assert.True(t, metricNames["chromium.page.task.duration"])
	assert.True(t, metricNames["chromium.page.js_heap.used_size"])
	assert.True(t, metricNames["chromium.page.js_heap.total_size"])
}

func TestScrapeTargetCounts(t *testing.T) {
	cfg := &Config{Endpoint: "ws://localhost:9222"}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	s := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	now := pcommon.NewTimestampFromTime(time.Now())

	targets := []TargetInfo{
		{TargetID: "t1", Type: "page", Title: "Page 1", URL: "https://example.com"},
		{TargetID: "t2", Type: "page", Title: "Page 2", URL: "https://example.org"},
		{TargetID: "t3", Type: "service_worker", Title: "SW", URL: "https://example.com/sw.js"},
		{TargetID: "t4", Type: "iframe", Title: "Frame", URL: "https://example.com/frame"},
	}

	s.scrapeTargetCounts(now, targets)

	metrics := s.mb.Emit()
	require.Greater(t, metrics.ResourceMetrics().Len(), 0)

	rm := metrics.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics().At(0)

	found := false
	for i := 0; i < sm.Metrics().Len(); i++ {
		m := sm.Metrics().At(i)
		if m.Name() == "chromium.targets.count" {
			found = true
			assert.Greater(t, m.Gauge().DataPoints().Len(), 0)
		}
	}
	assert.True(t, found, "Expected chromium.targets.count metric")
}
