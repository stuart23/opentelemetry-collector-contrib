// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package playwrightreceiver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver/internal/metadata"
)

const playwrightServerScript = "/Users/stu/synthesizer/k8s/components/playwright/base/server.js"

// startPlaywrightServer launches the production Playwright server.js on a
// random port and returns the ws:// endpoint. The process is killed on cleanup.
func startPlaywrightServer(t *testing.T) string {
	t.Helper()

	if endpoint := os.Getenv("PLAYWRIGHT_ENDPOINT"); endpoint != "" {
		t.Logf("Using endpoint from PLAYWRIGHT_ENDPOINT env: %s", endpoint)
		return endpoint
	}

	port := findFreePort(t)

	// Resolve the local node_modules so server.js can find the playwright package.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	nodeModules := filepath.Join(cwd, "node_modules")

	cmd := exec.Command("node", playwrightServerScript)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PLAYWRIGHT_PORT=%d", port),
		fmt.Sprintf("NODE_PATH=%s", nodeModules),
	)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	deadline := time.After(15 * time.Second)
	ready := make(chan struct{})
	var endpoint string

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "WebSocket endpoint:") {
				endpoint = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				// Normalize — server.js prints host as 0.0.0.0, we connect via 127.0.0.1
				endpoint = strings.Replace(endpoint, "0.0.0.0", "127.0.0.1", 1)
				close(ready)
				return
			}
		}
	}()

	select {
	case <-ready:
		t.Logf("Playwright server ready at %s", endpoint)
	case <-deadline:
		_ = cmd.Process.Kill()
		t.Fatal("server.js did not print WebSocket endpoint within 15s")
	}

	return endpoint
}

func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var allExpectedMetrics = []string{
	"playwright.targets.count",
	"playwright.page.document.count",
	"playwright.page.frame.count",
	"playwright.page.js_event_listener.count",
	"playwright.page.dom_node.count",
	"playwright.page.layout.count",
	"playwright.page.recalc_style.count",
	"playwright.page.layout.duration",
	"playwright.page.recalc_style.duration",
	"playwright.page.script.duration",
	"playwright.page.task.duration",
	"playwright.page.js_heap.used_size",
	"playwright.page.js_heap.total_size",
}

func collectMetricNames(metrics pmetric.Metrics) map[string]bool {
	names := make(map[string]bool)
	for i := 0; i < metrics.ResourceMetrics().Len(); i++ {
		rm := metrics.ResourceMetrics().At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			for k := 0; k < sm.Metrics().Len(); k++ {
				names[sm.Metrics().At(k).Name()] = true
			}
		}
	}
	return names
}

// TestIntegrationAllPerformanceMetrics verifies the receiver emits all
// Performance.getMetrics data as proper OTel metrics against a real Chromium.
func TestIntegrationAllPerformanceMetrics(t *testing.T) {
	endpoint := startPlaywrightServer(t)

	cfg := &Config{Endpoint: endpoint}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := scraper.start(ctx, componenttest.NewNopHost())
	require.NoError(t, err, "Failed to connect to %s", endpoint)
	defer scraper.shutdown(ctx)

	metrics, err := scraper.scrape(ctx)
	require.NoError(t, err, "Scrape failed")
	require.Greater(t, metrics.ResourceMetrics().Len(), 0, "Expected resource metrics")

	names := collectMetricNames(metrics)
	t.Logf("Emitted %d distinct metric names:", len(names))
	for name := range names {
		t.Logf("  %s", name)
	}

	for _, expected := range allExpectedMetrics {
		assert.True(t, names[expected], "Missing metric: %s", expected)
	}
}

// TestIntegrationTwoPages opens two pages through the Playwright client,
// scrapes, and asserts that we get performance metrics for both pages and
// that playwright.targets.count includes at least 2 "page" targets.
func TestIntegrationTwoPages(t *testing.T) {
	endpoint := startPlaywrightServer(t)

	cfg := &Config{Endpoint: endpoint}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := scraper.start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)
	defer scraper.shutdown(ctx)

	// The scraper already created 1 monitoring page during init.
	// Create a second page in the same context.
	pages := scraper.client.Pages()
	require.GreaterOrEqual(t, len(pages), 1, "Expected at least 1 page after init")

	existingPage := pages[0]
	t.Logf("Existing page: GUID=%s context=%s", existingPage.GUID, existingPage.ContextGUID)

	// Navigate the first page to a real URL
	if navErr := scraper.client.NavigatePage(ctx, existingPage.GUID, "https://example.com"); navErr != nil {
		t.Logf("Navigate page 1: %v (continuing anyway)", navErr)
	}
	time.Sleep(500 * time.Millisecond)

	// Open a second page in the same BrowserContext
	page2GUID, err := scraper.client.NewPage(ctx, existingPage.ContextGUID)
	require.NoError(t, err, "Failed to create second page")
	t.Logf("Created second page: GUID=%s", page2GUID)

	time.Sleep(500 * time.Millisecond)

	// Navigate the second page to a different URL
	if navErr := scraper.client.NavigatePage(ctx, page2GUID, "https://example.org"); navErr != nil {
		t.Logf("Navigate page 2: %v (continuing anyway)", navErr)
	}
	time.Sleep(500 * time.Millisecond)

	// Verify the client now tracks 2 pages
	pages = scraper.client.Pages()
	require.Equal(t, 2, len(pages), "Expected exactly 2 tracked pages")
	t.Logf("Tracked pages:")
	for _, p := range pages {
		t.Logf("  GUID=%s url=%s", p.GUID, p.URL)
	}

	// Scrape and verify metrics
	metrics, err := scraper.scrape(ctx)
	require.NoError(t, err)
	require.Greater(t, metrics.ResourceMetrics().Len(), 0)

	rm := metrics.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics().At(0)

	// --- Check playwright.targets.count has >= 2 page targets ---
	var totalPageTargets int64
	for i := 0; i < sm.Metrics().Len(); i++ {
		m := sm.Metrics().At(i)
		if m.Name() != "playwright.targets.count" {
			continue
		}
		for j := 0; j < m.Gauge().DataPoints().Len(); j++ {
			dp := m.Gauge().DataPoints().At(j)
			tt, _ := dp.Attributes().Get("target.type")
			t.Logf("targets.count  type=%-20s count=%d", tt.Str(), dp.IntValue())
			if tt.Str() == "page" {
				totalPageTargets += dp.IntValue()
			}
		}
	}
	assert.GreaterOrEqual(t, totalPageTargets, int64(2),
		"Expected at least 2 page targets, got %d", totalPageTargets)

	// --- Check performance metrics have data points for 2 pages ---
	heapMetricDPs := 0
	for i := 0; i < sm.Metrics().Len(); i++ {
		m := sm.Metrics().At(i)
		if m.Name() != "playwright.page.js_heap.used_size" {
			continue
		}
		heapMetricDPs = m.Gauge().DataPoints().Len()
		for j := 0; j < m.Gauge().DataPoints().Len(); j++ {
			dp := m.Gauge().DataPoints().At(j)
			url, _ := dp.Attributes().Get("target.url")
			t.Logf("js_heap.used_size  url=%-50s value=%d", url.Str(), dp.IntValue())
		}
	}
	assert.Equal(t, 2, heapMetricDPs,
		"Expected 2 data points for js_heap.used_size (one per page), got %d", heapMetricDPs)
}

// TestIntegrationMultipleScrapes verifies stability over multiple scrape cycles.
func TestIntegrationMultipleScrapes(t *testing.T) {
	endpoint := startPlaywrightServer(t)

	cfg := &Config{Endpoint: endpoint}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := scraper.start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)
	defer scraper.shutdown(ctx)

	const numScrapes = 3
	for i := 0; i < numScrapes; i++ {
		t.Run(fmt.Sprintf("scrape_%d", i+1), func(t *testing.T) {
			metrics, scrapeErr := scraper.scrape(ctx)
			require.NoError(t, scrapeErr, "Scrape %d failed", i+1)
			require.Greater(t, metrics.ResourceMetrics().Len(), 0, "Scrape %d returned no metrics", i+1)

			names := collectMetricNames(metrics)
			assert.True(t, names["playwright.targets.count"])
			t.Logf("Scrape %d: %d metric names emitted", i+1, len(names))
		})

		if i < numScrapes-1 {
			time.Sleep(2 * time.Second)
		}
	}
}

// TestIntegrationFullReceiverPipeline tests the full receiver → consumer pipeline.
func TestIntegrationFullReceiverPipeline(t *testing.T) {
	endpoint := startPlaywrightServer(t)

	cfg := createDefaultConfig().(*Config)
	cfg.Endpoint = endpoint
	cfg.CollectionInterval = 2 * time.Second

	consumer := new(consumertest.MetricsSink)
	recv, err := createMetricsReceiver(context.Background(), receivertest.NewNopSettings(metadata.Type), cfg, consumer)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = recv.Start(ctx, componenttest.NewNopHost())
	require.NoError(t, err, "Receiver failed to start")
	defer recv.Shutdown(context.Background())

	require.Eventually(t, func() bool {
		return consumer.DataPointCount() > 0
	}, 15*time.Second, 500*time.Millisecond, "No metrics collected within timeout")

	allMetrics := consumer.AllMetrics()
	require.Greater(t, len(allMetrics), 0)

	names := collectMetricNames(allMetrics[0])
	t.Logf("Pipeline collected %d metric names from %d batches", len(names), len(allMetrics))

	for _, expected := range allExpectedMetrics {
		assert.True(t, names[expected], "Pipeline missing metric: %s", expected)
	}
}
