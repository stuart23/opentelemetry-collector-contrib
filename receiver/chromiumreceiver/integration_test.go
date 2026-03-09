// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package chromiumreceiver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

// startChrome launches headless Chrome with --remote-debugging-port on a
// random port and returns the ws:// endpoint. The process is killed on cleanup.
func startChrome(t *testing.T) string {
	t.Helper()

	if endpoint := os.Getenv("CDP_ENDPOINT"); endpoint != "" {
		t.Logf("Using endpoint from CDP_ENDPOINT env: %s", endpoint)
		return endpoint
	}

	port := findFreePort(t)

	candidates := []string{
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}

	var chromePath string
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			chromePath = p
			break
		}
	}
	if chromePath == "" {
		t.Skip("No Chrome/Chromium binary found; skipping integration test")
	}

	cmd := exec.Command(chromePath,
		"--headless",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"about:blank",
	)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	endpoint := fmt.Sprintf("ws://127.0.0.1:%d", port)
	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("Chrome did not start listening within 15s")
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Logf("Chrome ready at %s", endpoint)
				time.Sleep(500 * time.Millisecond)
				return endpoint
			}
		}
	}
}

func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var allExpectedMetrics = []string{
	"chromium.targets.count",
	"chromium.page.document.count",
	"chromium.page.frame.count",
	"chromium.page.js_event_listener.count",
	"chromium.page.dom_node.count",
	"chromium.page.layout.count",
	"chromium.page.recalc_style.count",
	"chromium.page.layout.duration",
	"chromium.page.recalc_style.duration",
	"chromium.page.script.duration",
	"chromium.page.task.duration",
	"chromium.page.js_heap.used_size",
	"chromium.page.js_heap.total_size",
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

func TestIntegrationAllPerformanceMetrics(t *testing.T) {
	endpoint := startChrome(t)

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

func TestIntegrationMultipleScrapes(t *testing.T) {
	endpoint := startChrome(t)

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
			assert.True(t, names["chromium.targets.count"])
			t.Logf("Scrape %d: %d metric names emitted", i+1, len(names))
		})

		if i < numScrapes-1 {
			time.Sleep(2 * time.Second)
		}
	}
}

func TestIntegrationFullReceiverPipeline(t *testing.T) {
	endpoint := startChrome(t)

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
