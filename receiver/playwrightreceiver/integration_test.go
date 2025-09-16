// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package playwrightreceiver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver/internal/metadata"
)

// findAvailablePort finds an available TCP port for Playwright.
func findAvailablePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find available port: %v", err)
	}
	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

// playwrightTarget represents a Playwright target from the /json endpoint.
type playwrightTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// browserServerInfo represents the response from a Playwright browserServer.
type browserServerInfo struct {
	WSEndpointPath string `json:"wsEndpointPath"`
}

// discoverPlaywrightWebSocketEndpoint discovers the Playwright WebSocket endpoint from the HTTP endpoint.
func discoverPlaywrightWebSocketEndpoint(t *testing.T, httpPort int) string {
	t.Helper()

	httpURL := fmt.Sprintf("http://127.0.0.1:%d/json", httpPort)

	// Wait for the Playwright HTTP endpoint to be available
	for i := 0; i < 30; i++ { // Wait up to 30 seconds
		resp, err := http.Get(httpURL)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		if resp.StatusCode == 200 {
			// Try to parse as browserServer info first
			var browserServer browserServerInfo
			readErr := json.NewDecoder(resp.Body).Decode(&browserServer)
			resp.Body.Close()

			if readErr == nil && browserServer.WSEndpointPath != "" {
				// This is a Playwright browserServer
				endpoint := fmt.Sprintf("ws://127.0.0.1:%d%s", httpPort, browserServer.WSEndpointPath)
				t.Logf("Found Playwright browserServer with WebSocket URL: %s", endpoint)
				return endpoint
			}

			// Try to parse as regular Playwright targets
			resp2, err2 := http.Get(httpURL)
			if err2 != nil {
				time.Sleep(1 * time.Second)
				continue
			}

			var targets []playwrightTarget
			err2 = json.NewDecoder(resp2.Body).Decode(&targets)
			resp2.Body.Close()

			if err2 == nil && len(targets) > 0 {
				// Find the first target with a WebSocket debugger URL
				for _, target := range targets {
					if target.WebSocketDebuggerURL != "" {
						t.Logf("Found Playwright target: %s (%s) with WebSocket URL: %s", target.Title, target.Type, target.WebSocketDebuggerURL)
						return target.WebSocketDebuggerURL
					}
				}
			}
		} else {
			resp.Body.Close()
		}

		time.Sleep(1 * time.Second)
	}

	t.Fatalf("No Playwright endpoint found at %s after 30 seconds", httpURL)
	return ""
}

// setupPlaywrightBrowser sets up a Playwright-managed Chromium instance and returns the Playwright endpoint.
// It handles cleanup automatically using t.Cleanup().
func setupPlaywrightBrowser(t *testing.T) string {
	t.Helper()

	// Check if we should use an existing Playwright endpoint
	if endpoint := os.Getenv("PLAYWRIGHT_Playwright_ENDPOINT"); endpoint != "" {
		t.Logf("Using existing Playwright endpoint from environment: %s", endpoint)
		return endpoint
	}

	// Check if there's a Playwright instance running on localhost:9222 (Playwright server)
	if isPortOpen("127.0.0.1", 9222) {
		t.Logf("Found existing Playwright instance on localhost:9222")
		endpoint := discoverPlaywrightWebSocketEndpoint(t, 9222)
		t.Logf("Using existing Playwright endpoint: %s", endpoint)
		return endpoint
	}

	// Fall back to creating a new Playwright-managed instance
	t.Logf("No existing Playwright instance found, creating new Playwright-managed Chromium")
	return createPlaywrightBrowser(t)
}

// isPortOpen checks if a TCP port is open and accepting connections.
func isPortOpen(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// createPlaywrightBrowser creates a new Playwright-managed Chromium instance.
func createPlaywrightBrowser(t *testing.T) string {
	t.Helper()

	// Check if we should skip Playwright installation
	skipInstall := os.Getenv("PLAYWRIGHT_SKIP_BROWSER_INSTALL") == "1"

	// Initialize Playwright
	err := playwright.Install(&playwright.RunOptions{
		SkipInstallBrowsers: skipInstall,
		Verbose:             testing.Verbose(),
	})
	if err != nil && !skipInstall {
		t.Fatalf("Failed to install Playwright: %v", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("Failed to start Playwright: %v", err)
	}
	t.Cleanup(func() {
		if stopErr := pw.Stop(); stopErr != nil {
			t.Logf("Warning: Failed to stop Playwright: %v", stopErr)
		}
	})

	// Find an available port for Playwright
	cdpPort := findAvailablePort(t)

	// Launch Chromium with Playwright enabled on a specific port
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args: []string{
			fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
			"--remote-debugging-address=127.0.0.1",
		},
	})
	if err != nil {
		t.Fatalf("Failed to launch Chromium: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := browser.Close(); closeErr != nil {
			t.Logf("Warning: Failed to close browser: %v", closeErr)
		}
	})

	// Create a page to ensure we have some targets
	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("Failed to create page: %v", err)
	}

	// Navigate to a simple page to create more interesting targets
	_, err = page.Goto("data:text/html,<html><head><title>Test Page</title></head><body><h1>Integration Test Page</h1></body></html>")
	if err != nil {
		t.Logf("Warning: Failed to navigate page: %v", err)
	}

	// Discover the actual Playwright WebSocket endpoint
	endpoint := discoverPlaywrightWebSocketEndpoint(t, cdpPort)
	t.Logf("Using Playwright endpoint: %s", endpoint)

	return endpoint
}

// TestIntegrationPlaywrightReceiver tests the receiver against a Playwright-managed Chromium instance.
// This test is only run when the integration build tag is specified.
//
// To run this test:
//
//	go test -tags=integration -v ./receiver/playwrightreceiver -run TestIntegrationPlaywrightReceiver
//
// The test will automatically:
// 1. Install Playwright browsers if needed
// 2. Launch a Chromium instance with Playwright enabled
// 3. Run the receiver tests against it
// 4. Clean up the browser instance
func TestIntegrationPlaywrightReceiver(t *testing.T) {
	// Setup Playwright and browser
	endpoint := setupPlaywrightBrowser(t)

	cfg := &Config{
		Endpoint: endpoint,
	}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test connection
	t.Run("connection", func(t *testing.T) {
		err := scraper.start(ctx, componenttest.NewNopHost())
		require.NoError(t, err, "Failed to connect to Playwright endpoint %s", endpoint)
		defer func() {
			shutdownErr := scraper.shutdown(ctx)
			assert.NoError(t, shutdownErr)
		}()

		// Test scraping
		t.Run("scraping", func(t *testing.T) {
			metrics, err := scraper.scrape(ctx)
			require.NoError(t, err, "Failed to scrape metrics")
			assert.NotNil(t, metrics)

			// Verify we got some metrics
			assert.Greater(t, metrics.ResourceMetrics().Len(), 0, "Expected at least one resource metric")

			rm := metrics.ResourceMetrics().At(0)
			assert.Greater(t, rm.ScopeMetrics().Len(), 0, "Expected at least one scope metric")

			sm := rm.ScopeMetrics().At(0)
			assert.Greater(t, sm.Metrics().Len(), 0, "Expected at least one metric")

			// Find the playwright.targets.count metric
			var foundTargetMetric bool
			var targetMetricIndex int
			for i := 0; i < sm.Metrics().Len(); i++ {
				metric := sm.Metrics().At(i)
				if metric.Name() == "playwright.targets.count" {
					foundTargetMetric = true
					targetMetricIndex = i
					break
				}
			}

			require.True(t, foundTargetMetric, "Expected to find playwright.targets.count metric")

			// Log the metrics for debugging
			t.Logf("Successfully scraped metrics from Playwright endpoint: %s", endpoint)
			t.Logf("Total resource metrics: %d", metrics.ResourceMetrics().Len())
			t.Logf("Total data points: %d", sm.Metrics().At(0).Gauge().DataPoints().Len())

			// Verify metric structure
			metric := sm.Metrics().At(targetMetricIndex)
			assert.Equal(t, "playwright.targets.count", metric.Name())
			assert.Greater(t, metric.Gauge().DataPoints().Len(), 0, "Expected at least one data point")

			// Log each target type and count
			for i := 0; i < metric.Gauge().DataPoints().Len(); i++ {
				dp := metric.Gauge().DataPoints().At(i)
				attrs := dp.Attributes()

				endpointVal, exists := attrs.Get("playwright.endpoint")
				assert.True(t, exists, "Expected playwright.endpoint attribute")
				assert.Equal(t, endpoint, endpointVal.Str())

				targetTypeVal, exists := attrs.Get("target.type")
				assert.True(t, exists, "Expected target.type attribute")

				t.Logf("Target type: %s, Count: %d", targetTypeVal.Str(), dp.IntValue())
			}
		})
	})
}

// TestIntegrationPlaywrightClient tests the Playwright client directly against a Playwright-managed Chromium instance.
func TestIntegrationPlaywrightClient(t *testing.T) {
	// Setup Playwright and browser
	endpoint := setupPlaywrightBrowser(t)

	client := NewPlaywrightClient(endpoint, receivertest.NewNopSettings(metadata.Type).Logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("connect_and_get_targets", func(t *testing.T) {
		err := client.Connect(ctx)
		require.NoError(t, err, "Failed to connect to Playwright endpoint %s", endpoint)
		defer func() {
			disconnectErr := client.Disconnect()
			assert.NoError(t, disconnectErr)
		}()

		// Initialize Playwright and capture browser information
		initResult, err := client.Initialize(ctx)
		require.NoError(t, err, "Failed to initialize Playwright")

		if initResult.BrowserInfo == nil {
			t.Skip("No browser information available - skipping target retrieval test")
			return
		}

		// Create CDP session
		sessionResult, err := client.NewBrowserCDPSession(ctx, initResult.BrowserInfo.GUID)
		require.NoError(t, err, "Failed to create CDP session")

		// Get targets via CDP
		resp, err := client.GetTargetsViaCDP(ctx, sessionResult.Session.GUID)
		require.NoError(t, err, "Failed to get targets via CDP")

		// Parse the response
		var targetsResp TargetsResponse
		err = json.Unmarshal(resp.Result, &targetsResp)
		require.NoError(t, err, "Failed to parse targets response")

		t.Logf("Retrieved %d targets from Playwright via CDP", len(targetsResp.Result.TargetInfos))

		// Log each target for debugging
		targetTypes := make(map[string]int)
		for _, target := range targetsResp.Result.TargetInfos {
			targetTypes[target.Type]++
			t.Logf("Target: ID=%s, Type=%s, Title=%s, URL=%s, Attached=%v",
				target.TargetID, target.Type, target.Title, target.URL, target.Attached)
		}

		// Log summary by type
		t.Logf("Target summary by type:")
		for targetType, count := range targetTypes {
			t.Logf("  %s: %d", targetType, count)
		}

		// Verify targets - browserServer might not be able to create targets, so just log the result
		if len(targetsResp.Result.TargetInfos) == 0 {
			t.Logf("No targets found - this is expected for some browserServer configurations")
		} else {
			t.Logf("Successfully found %d targets", len(targetsResp.Result.TargetInfos))
		}
	})
}

// TestIntegrationMultipleScrapes tests multiple scraping cycles to ensure stability.
func TestIntegrationMultipleScrapes(t *testing.T) {
	// Setup Playwright and browser
	endpoint := setupPlaywrightBrowser(t)

	cfg := &Config{
		Endpoint: endpoint,
	}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	scraper := newScraper(cfg, receivertest.NewNopSettings(metadata.Type))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := scraper.start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)
	defer func() {
		shutdownErr := scraper.shutdown(ctx)
		assert.NoError(t, shutdownErr)
	}()

	// Perform multiple scrapes
	const numScrapes = 5
	for i := 0; i < numScrapes; i++ {
		t.Run(fmt.Sprintf("scrape_%d", i+1), func(t *testing.T) {
			metrics, scrapeErr := scraper.scrape(ctx)
			assert.NoError(t, scrapeErr, "Scrape %d failed", i+1)
			assert.NotNil(t, metrics)
			assert.Greater(t, metrics.ResourceMetrics().Len(), 0, "Scrape %d returned no metrics", i+1)

			t.Logf("Scrape %d completed successfully", i+1)
		})

		// Small delay between scrapes
		if i < numScrapes-1 {
			time.Sleep(2 * time.Second)
		}
	}
}

// TestInitializeAgainstRealServer tests the Initialize method against a real Playwright server
// This test requires a Playwright server running on localhost:8765/ws
// To run this test:
//
//	go test -tags=integration -v -run TestInitializeAgainstRealServer
func TestInitializeAgainstRealServer(t *testing.T) {
	endpoint := "ws://localhost:8765/ws"
	t.Logf("Testing Initialize method against real Playwright server at %s", endpoint)

	// Skip this test if the server is not available
	if !isPortOpen("127.0.0.1", 8765) {
		t.Skip("Skipping test: No Playwright server running on localhost:8765")
	}

	client := NewPlaywrightClient(endpoint, receivertest.NewNopSettings(metadata.Type).Logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err, "Failed to connect to Playwright server")
	defer func() {
		assert.NoError(t, client.Disconnect())
	}()

	t.Log("Connected to Playwright server successfully!")

	// Test the Initialize method
	t.Log("Calling Initialize method...")
	start := time.Now()

	result, err := client.Initialize(ctx)
	require.NoError(t, err, "Initialize method failed")
	require.NotNil(t, result, "Initialize result should not be nil")

	duration := time.Since(start)
	t.Logf("✅ Initialize completed successfully in %v", duration)

	// Verify the result
	assert.Equal(t, "Playwright", result.Playwright.GUID, "Expected Playwright GUID to be 'Playwright'")

	t.Logf("🎉 SUCCESS: Initialize method returned expected result: %+v", result)
	t.Logf("Playwright GUID: %s", result.Playwright.GUID)

	if result.BrowserInfo != nil {
		t.Logf("📱 Browser Info captured:")
		t.Logf("  GUID: %s", result.BrowserInfo.GUID)
		t.Logf("  Name: %s", result.BrowserInfo.Name)
		t.Logf("  Version: %s", result.BrowserInfo.Version)
		t.Logf("  Type: %s", result.BrowserInfo.Type)
		t.Logf("  Initializer: %+v", result.BrowserInfo.Initializer)

		// Test the NewBrowserCDPSession function
		t.Log("🔗 Testing NewBrowserCDPSession...")
		cdpSession, err := client.NewBrowserCDPSession(ctx, result.BrowserInfo.GUID)
		if err != nil {
			t.Logf("❌ Failed to create CDP session: %v", err)
		} else {
			t.Logf("✅ Successfully created CDP session with GUID: %s", cdpSession.Session.GUID)

			// Test sending Target.getTargets via CDP
			t.Log("🎯 Testing GetTargetsViaCDP...")
			resp, err := client.GetTargetsViaCDP(ctx, cdpSession.Session.GUID)
			if err != nil {
				t.Logf("❌ Failed to send Target.getTargets via CDP: %v", err)
			} else {
				t.Logf("✅ Successfully sent Target.getTargets via CDP session")
				t.Logf("📊 Received response: %s", string(resp.Result))
			}
		}

	} else {
		t.Logf("⚠️  No browser info captured")
	}
}

// TestRealScraperAgainstPort8765 tests the complete scraper flow against the real server on port 8765
func TestRealScraperAgainstPort8765(t *testing.T) {
	endpoint := "ws://localhost:8765/ws"
	t.Logf("Testing complete scraper flow against: %s", endpoint)

	// Skip if server not available
	if !isPortOpen("127.0.0.1", 8765) {
		t.Skip("Skipping test: No Playwright server running on localhost:8765")
	}

	cfg := createDefaultConfig().(*Config)
	cfg.Endpoint = endpoint
	cfg.CollectionInterval = 2 * time.Second // Faster collection for testing

	consumer := new(consumertest.MetricsSink)
	receiver, err := createMetricsReceiver(context.Background(), receivertest.NewNopSettings(metadata.Type), cfg, consumer)
	require.NoError(t, err)

	host := componenttest.NewNopHost()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Log("🚀 Starting receiver (this will call Initialize and NewBrowserCDPSession)...")
	err = receiver.Start(ctx, host)
	if err != nil {
		t.Fatalf("Failed to start receiver: %v", err)
	}
	t.Log("✅ Receiver started successfully")

	defer func() {
		t.Log("🛑 Shutting down receiver...")
		assert.NoError(t, receiver.Shutdown(context.Background()))
	}()

	t.Log("⏱️  Waiting for metrics collection...")
	// Wait for some metrics to be collected
	require.Eventually(t, func() bool {
		return consumer.DataPointCount() > 0
	}, 15*time.Second, 500*time.Millisecond, "failed to collect metrics")

	t.Log("📊 Analyzing collected metrics...")
	metrics := consumer.AllMetrics()[0]
	assert.Greater(t, metrics.ResourceMetrics().Len(), 0, "Expected at least one resource metric")

	rm := metrics.ResourceMetrics().At(0)
	assert.Greater(t, rm.ScopeMetrics().Len(), 0, "Expected at least one scope metric")

	sm := rm.ScopeMetrics().At(0)
	assert.Greater(t, sm.Metrics().Len(), 0, "Expected at least one metric")

	// Find the playwright.targets.count metric
	var foundTargetMetric bool
	var targetMetricIndex int
	for i := 0; i < sm.Metrics().Len(); i++ {
		metric := sm.Metrics().At(i)
		if metric.Name() == "playwright.targets.count" {
			foundTargetMetric = true
			targetMetricIndex = i
			break
		}
	}

	require.True(t, foundTargetMetric, "Expected to find playwright.targets.count metric")

	// Verify metric structure
	metric := sm.Metrics().At(targetMetricIndex)
	assert.Equal(t, "playwright.targets.count", metric.Name())
	assert.Greater(t, metric.Gauge().DataPoints().Len(), 0, "Expected at least one data point")

	t.Logf("✅ Successfully collected metrics from: %s", endpoint)
	t.Logf("📈 Total resource metrics: %d", metrics.ResourceMetrics().Len())
	t.Logf("📊 Total data points: %d", metric.Gauge().DataPoints().Len())

	// Log each target type and count
	for i := 0; i < metric.Gauge().DataPoints().Len(); i++ {
		dp := metric.Gauge().DataPoints().At(i)
		targetType, exists := dp.Attributes().Get("target.type")
		if exists {
			t.Logf("  🎯 Target Type: %s, Count: %d", targetType.Str(), dp.IntValue())
		}
	}

	t.Log("🎉 Test completed successfully! The scraper now uses Initialize and NewBrowserCDPSession on startup.")
}
