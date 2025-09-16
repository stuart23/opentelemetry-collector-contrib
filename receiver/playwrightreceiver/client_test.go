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
