// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package playwrightreceiver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestPlaywrightClientConnect(t *testing.T) {
	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		// Read until connection closes
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	logger := zaptest.NewLogger(t)
	client := NewPlaywrightClient(wsURL, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	assert.NoError(t, err)

	err = client.Disconnect()
	assert.NoError(t, err)
}

func TestPlaywrightClientConnectFailure(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewPlaywrightClient("ws://localhost:99999", logger) // invalid endpoint

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to Playwright endpoint")
}

func TestPlaywrightClientDoubleConnect(t *testing.T) {
	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		// Read until connection closes
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	logger := zaptest.NewLogger(t)
	client := NewPlaywrightClient(wsURL, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	assert.NoError(t, err)

	// Second connect should not error
	err = client.Connect(ctx)
	assert.NoError(t, err)

	err = client.Disconnect()
	assert.NoError(t, err)
}

func TestPlaywrightClientDisconnectWithoutConnect(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewPlaywrightClient("ws://localhost:9222", logger)

	err := client.Disconnect()
	assert.NoError(t, err)
}

func TestPlaywrightClientInitialize(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Mock a successful initialize response
			if msg.Method == "initialize" {
				response := PlaywrightResponse{
					ID:     msg.ID,
					Result: json.RawMessage(`{"playwright": {"guid": "Playwright"}}`),
				}
				conn.WriteJSON(response)
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the Initialize method
	result, err := client.Initialize(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the result contains the expected Playwright GUID
	assert.Equal(t, "Playwright", result.Playwright.GUID)

	// Verify the message was sent correctly
	assert.Equal(t, "initialize", receivedMessage.Method)

	// Log the received message for debugging
	t.Logf("Received message: %+v", receivedMessage)

	// Convert maps to JSON for logging
	if receivedMessage.Params != nil {
		paramsJSON, _ := json.Marshal(receivedMessage.Params)
		t.Logf("Params: %s", string(paramsJSON))
	}
	if receivedMessage.Metadata != nil {
		metadataJSON, _ := json.Marshal(receivedMessage.Metadata)
		t.Logf("Metadata: %s", string(metadataJSON))
	}

	// Check the params contain sdkLanguage: python (if Params is not empty)
	if receivedMessage.Params != nil && len(receivedMessage.Params) > 0 {
		assert.Equal(t, "python", receivedMessage.Params["sdkLanguage"])
	}

	// Check the metadata contains wallTime, apiName, and internal (if Metadata is not empty)
	if receivedMessage.Metadata != nil && len(receivedMessage.Metadata) > 0 {
		assert.Contains(t, receivedMessage.Metadata, "wallTime")
		assert.Equal(t, "Connection.init", receivedMessage.Metadata["apiName"])
		assert.Equal(t, false, receivedMessage.Metadata["internal"])

		// Check that wallTime is a recent timestamp (within last 10 seconds)
		wallTime, ok := receivedMessage.Metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	t.Logf("Initialize method test successful!")
	t.Logf("Received message: %+v", receivedMessage)
	t.Logf("Result: %+v", result)
}

func TestPlaywrightClientNewBrowserCDPSession(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Mock a successful newBrowserCDPSession response
			if msg.Method == "newBrowserCDPSession" {
				response := PlaywrightResponse{
					ID:     msg.ID,
					Result: json.RawMessage(`{"session": {"guid": "session@test-guid-456"}}`),
				}
				conn.WriteJSON(response)
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the NewBrowserCDPSession method
	browserGUID := "browser@test-guid-123"
	result, err := client.NewBrowserCDPSession(ctx, browserGUID)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the result contains the expected session GUID
	assert.Equal(t, "session@test-guid-456", result.Session.GUID)

	// Verify the message was sent correctly
	assert.Equal(t, "newBrowserCDPSession", receivedMessage.Method)
	assert.Equal(t, browserGUID, receivedMessage.GUID)

	// Log the received message for debugging
	t.Logf("Received message: %+v", receivedMessage)

	// Convert metadata to JSON for logging
	if receivedMessage.Metadata != nil {
		metadataJSON, _ := json.Marshal(receivedMessage.Metadata)
		t.Logf("Metadata: %s", string(metadataJSON))
	}

	// Check the metadata contains the required fields
	if receivedMessage.Metadata != nil && len(receivedMessage.Metadata) > 0 {
		assert.Contains(t, receivedMessage.Metadata, "wallTime")
		assert.Equal(t, "Browser.new_browser_cdp_session", receivedMessage.Metadata["apiName"])
		assert.Equal(t, false, receivedMessage.Metadata["internal"])

		// Check that wallTime is a recent timestamp (within last 10 seconds)
		wallTime, ok := receivedMessage.Metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	t.Logf("NewBrowserCDPSession method test successful!")
	t.Logf("Received message: %+v", receivedMessage)
	t.Logf("Result: %+v", result)
}

func TestPlaywrightClientSendCDPMessage(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Send back a response for sendCDPMessage (now uses sendCommand which expects a response)
			if msg.Method == "send" {
				response := PlaywrightResponse{
					ID:     msg.ID,
					Result: []byte(`{"result":{"targetInfos":[{"targetId":"target-1","type":"page","title":"Test Page","url":"about:blank"}]}}`),
				}
				conn.WriteJSON(response)
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the sendCDPMessage method with a simple CDP command
	sessionGUID := "session@test-session-123"
	cdpMethod := "Target.getTargets"
	cdpParams := map[string]interface{}{
		"filter": []interface{}{map[string]interface{}{}},
	}

	resp, err := client.sendCDPMessage(ctx, sessionGUID, cdpMethod, cdpParams, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Give a moment for the message to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify the message was sent correctly
	assert.Equal(t, "send", receivedMessage.Method)
	assert.Equal(t, sessionGUID, receivedMessage.GUID)

	// Verify the params structure (method and params directly under params)
	assert.Contains(t, receivedMessage.Params, "method")
	assert.Contains(t, receivedMessage.Params, "params")

	assert.Equal(t, cdpMethod, receivedMessage.Params["method"])
	assert.Equal(t, cdpParams, receivedMessage.Params["params"])

	// Log the received message for debugging
	t.Logf("Received message: %+v", receivedMessage)

	// Convert metadata to JSON for logging
	if receivedMessage.Metadata != nil {
		metadataJSON, _ := json.Marshal(receivedMessage.Metadata)
		t.Logf("Metadata: %s", string(metadataJSON))
	}

	// Check the metadata contains the required fields
	if receivedMessage.Metadata != nil && len(receivedMessage.Metadata) > 0 {
		assert.Contains(t, receivedMessage.Metadata, "wallTime")
		assert.Equal(t, "CDPSession.send", receivedMessage.Metadata["apiName"])
		assert.Equal(t, false, receivedMessage.Metadata["internal"])

		// Check that wallTime is a recent timestamp (within last 10 seconds)
		wallTime, ok := receivedMessage.Metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	t.Logf("sendCDPMessage method test successful!")
	t.Logf("CDP Method: %s", cdpMethod)
	t.Logf("Session GUID: %s", sessionGUID)
	t.Logf("Direct params: %+v", receivedMessage.Params)
	t.Logf("Response received: %+v", resp)
	t.Logf("Response result: %s", string(resp.Result))
}

func TestPlaywrightClientGetTargetsViaCDP(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Send back a response for GetTargetsViaCDP (now uses sendCommand which expects a response)
			if msg.Method == "send" {
				response := PlaywrightResponse{
					ID:     msg.ID,
					Result: []byte(`{"result":{"targetInfos":[{"targetId":"target-1","type":"page","title":"Test Page","url":"about:blank"}]}}`),
				}
				conn.WriteJSON(response)
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the GetTargetsViaCDP method
	sessionGUID := "session@test-cdp-session-789"

	resp, err := client.GetTargetsViaCDP(ctx, sessionGUID)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Give a moment for the message to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify the message was sent correctly
	assert.Equal(t, "send", receivedMessage.Method)
	assert.Equal(t, sessionGUID, receivedMessage.GUID)

	// Verify the params structure (method and params directly under params)
	assert.Contains(t, receivedMessage.Params, "method")
	assert.Contains(t, receivedMessage.Params, "params")

	assert.Equal(t, "Target.getTargets", receivedMessage.Params["method"])

	// Verify the Target.getTargets params structure
	cdpParamsObj, ok := receivedMessage.Params["params"].(map[string]interface{})
	require.True(t, ok, "CDP params should be a map")

	filter, exists := cdpParamsObj["filter"]
	require.True(t, exists, "filter should exist in CDP params")

	filterArray, ok := filter.([]interface{})
	require.True(t, ok, "filter should be an array")
	require.Len(t, filterArray, 1, "filter should have one element")

	filterElement, ok := filterArray[0].(map[string]interface{})
	require.True(t, ok, "filter element should be a map")
	require.Len(t, filterElement, 0, "filter element should be empty map")

	// Log the received message for debugging
	t.Logf("Received message: %+v", receivedMessage)

	// Convert metadata to JSON for logging
	if receivedMessage.Metadata != nil {
		metadataJSON, _ := json.Marshal(receivedMessage.Metadata)
		t.Logf("Metadata: %s", string(metadataJSON))
	}

	// Check the metadata contains the required fields
	if receivedMessage.Metadata != nil && len(receivedMessage.Metadata) > 0 {
		assert.Contains(t, receivedMessage.Metadata, "wallTime")
		assert.Equal(t, "CDPSession.send", receivedMessage.Metadata["apiName"])
		assert.Equal(t, false, receivedMessage.Metadata["internal"])

		// Check that wallTime is a recent timestamp (within last 10 seconds)
		wallTime, ok := receivedMessage.Metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	t.Logf("GetTargetsViaCDP method test successful!")
	t.Logf("Session GUID: %s", sessionGUID)
	t.Logf("CDP Method: Target.getTargets")
	t.Logf("Direct params: %+v", receivedMessage.Params)
	t.Logf("Response received: %+v", resp)
	t.Logf("Response result: %s", string(resp.Result))
}

func TestPlaywrightClientAttachToTarget(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Send back a response for AttachToTarget
			if msg.Method == "send" {
				if params, ok := msg.Params["method"].(string); ok && params == "Target.attachToTarget" {
					response := PlaywrightResponse{
						ID:     msg.ID,
						Result: []byte(`{"result":{"sessionId":"attached-session-123"}}`),
					}
					conn.WriteJSON(response)
				}
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the AttachToTarget method
	sessionGUID := "session@test-cdp-session-789"
	targetID := "target-123"

	result, err := client.AttachToTarget(ctx, sessionGUID, targetID)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Give a moment for the message to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify the message was sent correctly
	assert.Equal(t, "send", receivedMessage.Method, "Method should be 'send'")
	assert.Equal(t, sessionGUID, receivedMessage.GUID, "GUID should match session GUID")

	// Verify params structure
	assert.Contains(t, receivedMessage.Params, "method", "Params should contain 'method'")
	assert.Equal(t, "Target.attachToTarget", receivedMessage.Params["method"], "CDP method should be Target.attachToTarget")

	assert.Contains(t, receivedMessage.Params, "params", "Params should contain 'params'")
	cdpParams, ok := receivedMessage.Params["params"].(map[string]interface{})
	require.True(t, ok, "CDP params should be a map")

	assert.Equal(t, targetID, cdpParams["targetId"], "targetId parameter should match")
	assert.Equal(t, true, cdpParams["flatten"], "flatten parameter should be true")

	// Verify metadata
	if receivedMessage.Metadata != nil {
		metadata := receivedMessage.Metadata
		assert.Contains(t, metadata, "wallTime", "Metadata should contain wallTime")
		assert.Contains(t, metadata, "apiName", "Metadata should contain apiName")
		assert.Equal(t, "CDPSession.send", metadata["apiName"], "apiName should be CDPSession.send")
		assert.Equal(t, false, metadata["internal"], "internal should be false")

		// Verify wallTime is recent
		wallTime, ok := metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	// Verify the result
	assert.Equal(t, "attached-session-123", result.SessionID, "SessionID should match response")

	t.Logf("AttachToTarget method test successful!")
	t.Logf("Session GUID: %s", sessionGUID)
	t.Logf("Target ID: %s", targetID)
	t.Logf("CDP Method: Target.attachToTarget")
	t.Logf("Direct params: %+v", receivedMessage.Params)
	t.Logf("Result: %+v", result)
	t.Logf("Attached session ID: %s", result.SessionID)
}

func TestPlaywrightClientGetPerformanceMetrics(t *testing.T) {
	var receivedMessage PlaywrightMessage

	server := mockPlaywrightServer(t, func(conn *websocket.Conn) {
		for {
			var msg PlaywrightMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				return
			}

			receivedMessage = msg

			// Send back a response for GetPerformanceMetrics
			if msg.Method == "send" {
				if params, ok := msg.Params["method"].(string); ok && params == "Performance.getMetrics" {
					response := PlaywrightResponse{
						ID:     msg.ID,
						Result: []byte(`{"result":{"metrics":[{"name":"Timestamp","value":1234567.89},{"name":"Documents","value":3},{"name":"Frames","value":2},{"name":"JSEventListeners","value":15}]}}`),
					}
					conn.WriteJSON(response)
				}
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := NewPlaywrightClient(wsURL, zaptest.NewLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect()

	// Test the GetPerformanceMetrics method
	targetSessionID := "attached-session-123"

	result, err := client.GetPerformanceMetrics(ctx, targetSessionID)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Give a moment for the message to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify the message was sent correctly
	assert.Equal(t, "send", receivedMessage.Method, "Method should be 'send'")
	assert.Equal(t, targetSessionID, receivedMessage.GUID, "GUID should match target session ID")

	// Verify params structure
	assert.Contains(t, receivedMessage.Params, "method", "Params should contain 'method'")
	assert.Equal(t, "Performance.getMetrics", receivedMessage.Params["method"], "CDP method should be Performance.getMetrics")

	assert.Contains(t, receivedMessage.Params, "params", "Params should contain 'params'")
	cdpParams, ok := receivedMessage.Params["params"].(map[string]interface{})
	require.True(t, ok, "CDP params should be a map")

	// Performance.getMetrics should have empty params
	assert.Empty(t, cdpParams, "Performance.getMetrics should have empty params")

	// Verify metadata
	if receivedMessage.Metadata != nil {
		metadata := receivedMessage.Metadata
		assert.Contains(t, metadata, "wallTime", "Metadata should contain wallTime")
		assert.Contains(t, metadata, "apiName", "Metadata should contain apiName")
		assert.Equal(t, "CDPSession.send", metadata["apiName"], "apiName should be CDPSession.send")
		assert.Equal(t, false, metadata["internal"], "internal should be false")

		// Verify wallTime is recent
		wallTime, ok := metadata["wallTime"].(float64)
		require.True(t, ok, "wallTime should be a number")
		now := time.Now().UnixMilli()
		assert.InDelta(t, float64(now), wallTime, 10000, "wallTime should be recent")
	}

	// Verify the result contains expected performance metrics
	assert.Len(t, result.Metrics, 4, "Should have 4 performance metrics")

	expectedMetrics := map[string]float64{
		"Timestamp":        1234567.89,
		"Documents":        3,
		"Frames":           2,
		"JSEventListeners": 15,
	}

	for _, metric := range result.Metrics {
		expectedValue, exists := expectedMetrics[metric.Name]
		assert.True(t, exists, "Metric %s should be expected", metric.Name)
		assert.Equal(t, expectedValue, metric.Value, "Metric %s should have correct value", metric.Name)
	}

	t.Logf("GetPerformanceMetrics method test successful!")
	t.Logf("Target Session ID: %s", targetSessionID)
	t.Logf("CDP Method: Performance.getMetrics")
	t.Logf("Direct params: %+v", receivedMessage.Params)
	t.Logf("Result: %+v", result)
	t.Logf("Performance metrics count: %d", len(result.Metrics))
	for _, metric := range result.Metrics {
		t.Logf("  %s: %f", metric.Name, metric.Value)
	}
}
