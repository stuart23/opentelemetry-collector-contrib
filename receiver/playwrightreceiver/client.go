// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package playwrightreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver"

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// PlaywrightMessage represents a Playwright WebSocket message
type PlaywrightMessage struct {
	ID       int                    `json:"id"`
	GUID     string                 `json:"guid"`
	Method   string                 `json:"method"`
	Params   map[string]interface{} `json:"params,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// PlaywrightResponse represents a Playwright WebSocket response
type PlaywrightResponse struct {
	ID     int              `json:"id"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *PlaywrightError `json:"error,omitempty"`
}

// PlaywrightEvent represents a Playwright WebSocket event (like Browser creation)
type PlaywrightEvent struct {
	GUID   string                 `json:"guid"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}

// PlaywrightError represents an error in Playwright response
type PlaywrightError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TargetInfo represents information about a browser target
type TargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Attached         bool   `json:"attached"`
	CanAccessOpener  bool   `json:"canAccessOpener"`
	BrowserContextID string `json:"browserContextId"`
}

// GetTargetsResult represents the result of Target.getTargets command
type GetTargetsResult struct {
	TargetInfos []TargetInfo `json:"targetInfos"`
}

// ReconnectCallback is called when a reconnection happens
type ReconnectCallback func() error

// PlaywrightClient handles the Playwright WebSocket connection
type PlaywrightClient struct {
	conn              *websocket.Conn
	endpoint          string
	logger            *zap.Logger
	mu                sync.Mutex
	nextID            int
	responses         map[int]chan PlaywrightResponse
	closed            bool
	closeChan         chan struct{}
	reconnectChan     chan struct{}
	autoReconnect     bool
	reconnectCallback ReconnectCallback
}

// NewPlaywrightClient creates a new Playwright client
func NewPlaywrightClient(endpoint string, logger *zap.Logger) *PlaywrightClient {
	return &PlaywrightClient{
		endpoint:      endpoint,
		logger:        logger,
		responses:     make(map[int]chan PlaywrightResponse),
		closeChan:     make(chan struct{}),
		reconnectChan: make(chan struct{}, 1),
		autoReconnect: true,
	}
}

// Connect establishes a WebSocket connection to the Playwright endpoint
func (c *PlaywrightClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil // Already connected
	}

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Playwright endpoint %s: %w", c.endpoint, err)
	}

	c.conn = conn
	c.closed = false

	// Start reading responses in a goroutine
	go c.readResponses()

	// Start the reconnection handler goroutine
	go c.handleReconnection()

	return nil
}

// SetAutoReconnect enables or disables automatic reconnection
func (c *PlaywrightClient) SetAutoReconnect(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoReconnect = enabled
}

// SetReconnectCallback sets a callback function to be called after successful reconnection
func (c *PlaywrightClient) SetReconnectCallback(callback ReconnectCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconnectCallback = callback
}

// Disconnect closes the WebSocket connection
func (c *PlaywrightClient) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.closed {
		return nil
	}

	c.closed = true
	close(c.closeChan)

	// Close all pending response channels
	for _, ch := range c.responses {
		close(ch)
	}
	c.responses = make(map[int]chan PlaywrightResponse)

	err := c.conn.Close()
	c.conn = nil
	return err
}

// handleReconnection handles automatic reconnection when the WebSocket connection is lost
func (c *PlaywrightClient) handleReconnection() {
	for {
		select {
		case <-c.reconnectChan:
			c.logger.Info("Starting automatic reconnection...")

			// Wait a bit before attempting to reconnect
			time.Sleep(2 * time.Second)

			// Attempt to reconnect
			if err := c.reconnectInternal(); err != nil {
				c.logger.Error("Failed to reconnect", zap.Error(err))

				// Schedule another reconnection attempt
				select {
				case c.reconnectChan <- struct{}{}:
				default:
				}
			} else {
				c.logger.Info("Successfully reconnected to Playwright server")

				// Call reconnect callback if set
				c.mu.Lock()
				callback := c.reconnectCallback
				c.mu.Unlock()

				if callback != nil {
					if err := callback(); err != nil {
						c.logger.Error("Reconnect callback failed", zap.Error(err))
					} else {
						c.logger.Info("Reconnect callback completed successfully")
					}
				}
			}

		case <-c.closeChan:
			c.logger.Debug("Reconnection handler shutting down")
			return
		}
	}
}

// reconnectInternal performs the actual reconnection logic
func (c *PlaywrightClient) reconnectInternal() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close existing connection if any
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	// Don't reconnect if client is explicitly closed
	if c.closed {
		return fmt.Errorf("client is closed")
	}

	// Create a context with timeout for reconnection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt to establish new connection
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to reconnect to Playwright endpoint %s: %w", c.endpoint, err)
	}

	// Update connection and restart reader
	c.conn = conn
	go c.readResponses()

	return nil
}

// sendMessage sends a Playwright message and waits for response
func (c *PlaywrightClient) sendMessage(ctx context.Context, guid string, method string, params interface{}, metadata interface{}) (*PlaywrightResponse, error) {
	c.mu.Lock()
	if c.conn == nil || c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection is not established")
	}

	id := c.nextID
	c.nextID++

	respChan := make(chan PlaywrightResponse, 1)
	c.responses[id] = respChan
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.responses, id)
		close(respChan)
		c.mu.Unlock()
	}()

	// Convert params to map[string]interface{}
	var paramsMap map[string]interface{}
	if params != nil {
		if pm, ok := params.(map[string]interface{}); ok {
			paramsMap = pm
		} else if ps, ok := params.(map[string]string); ok {
			// Convert map[string]string to map[string]interface{}
			paramsMap = make(map[string]interface{})
			for k, v := range ps {
				paramsMap[k] = v
			}
		} else {
			return nil, fmt.Errorf("params must be map[string]interface{} or map[string]string")
		}
	}

	// Convert metadata to map[string]interface{}
	var metadataMap map[string]interface{}
	if metadata != nil {
		if mm, ok := metadata.(map[string]interface{}); ok {
			metadataMap = mm
		} else {
			return nil, fmt.Errorf("metadata must be map[string]interface{}")
		}
	}

	msg := PlaywrightMessage{
		ID:       id,
		GUID:     guid,
		Method:   method,
		Params:   paramsMap,
		Metadata: metadataMap,
	}

	// Send the message
	c.mu.Lock()
	err := c.conn.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to send Playwright message: %w", err)
	}

	// Wait for response
	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("Playwright error: %s (code: %d)", resp.Error.Message, resp.Error.Code)
		}
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeChan:
		return nil, fmt.Errorf("connection closed")
	}
}

// readResponses reads responses from the WebSocket connection
func (c *PlaywrightClient) readResponses() {
	for {
		c.mu.Lock()
		if c.conn == nil || c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		// Read raw message first to determine if it's a response or event
		_, rawMessage, err := c.conn.ReadMessage()
		if err != nil {
			c.logger.Warn("WebSocket connection lost", zap.Error(err))

			// Trigger reconnection if auto-reconnect is enabled
			c.mu.Lock()
			if c.autoReconnect && !c.closed {
				c.logger.Info("Triggering automatic reconnection...")
				select {
				case c.reconnectChan <- struct{}{}:
				default:
					// Channel is full, reconnection already pending
				}
			}
			c.mu.Unlock()
			return
		}

		// Try to parse as response first
		var resp PlaywrightResponse
		if err := json.Unmarshal(rawMessage, &resp); err == nil && (resp.ID != 0 || len(resp.Result) > 0 || resp.Error != nil) {
			// This is a response message
			c.mu.Lock()
			// First, try to send to the specific ID channel
			if respChan, exists := c.responses[resp.ID]; exists {
				select {
				case respChan <- resp:
				default:
					c.logger.Warn("Response channel full, dropping response", zap.Int("id", resp.ID))
				}
			} else {
				c.logger.Debug("Received response for unknown ID", zap.Int("id", resp.ID))
			}

			// Also send to the initialize channel if it exists and the response has a result
			if len(resp.Result) > 0 {
				if initChan, exists := c.responses[-1]; exists {
					select {
					case initChan <- resp:
					default:
						// Don't log warning for initialize channel, it's expected to be busy
					}
				}
			}
			c.mu.Unlock()
		} else {
			// Try to parse as event
			var event PlaywrightEvent
			if err := json.Unmarshal(rawMessage, &event); err == nil && event.Method != "" {
				// This is an event message - forward to initialize channel if it exists
				c.mu.Lock()
				if initChan, exists := c.responses[-1]; exists {
					// Convert event to response format for the initialize handler
					eventResp := PlaywrightResponse{
						ID:     -999,       // Special ID to indicate this is an event
						Result: rawMessage, // Pass the raw event data
					}
					select {
					case initChan <- eventResp:
					default:
						// Don't log warning for initialize channel, it's expected to be busy
					}
				}
				c.mu.Unlock()
			} else {
				c.logger.Debug("Received unknown message format", zap.String("message", string(rawMessage)))
			}
		}
	}
}

// CreateTargetParams represents parameters for Target.createTarget
type CreateTargetParams struct {
	URL string `json:"url"`
}

// CreateTargetResult represents the result of Target.createTarget
type CreateTargetResult struct {
	TargetID string `json:"targetId"`
}

// InitializeResult represents the result of the initialize command
type InitializeResult struct {
	Playwright struct {
		GUID string `json:"guid"`
	} `json:"playwright"`
	BrowserInfo *BrowserInfo `json:"browserInfo,omitempty"`
}

// BrowserInfo represents information about the browser instance
type BrowserInfo struct {
	GUID        string                 `json:"guid"`
	Version     string                 `json:"version,omitempty"`
	Name        string                 `json:"name,omitempty"`
	Type        string                 `json:"type,omitempty"`
	Initializer map[string]interface{} `json:"initializer,omitempty"`
}

// NewBrowserCDPSessionResult represents the result of newBrowserCDPSession
type NewBrowserCDPSessionResult struct {
	Session struct {
		GUID string `json:"guid"`
	} `json:"session"`
}

// Initialize calls the initialize Playwright method and waits for the Playwright object to be ready
func (c *PlaywrightClient) Initialize(ctx context.Context) (*InitializeResult, error) {
	// Create a special response channel to listen for any response with a result field
	initRespChan := make(chan PlaywrightResponse, 50) // Buffer for multiple setup messages

	// Add a temporary "catch-all" response handler that forwards any response with a result
	c.mu.Lock()
	// Use a negative ID that won't conflict with normal message IDs
	initResponseID := -1
	c.responses[initResponseID] = initRespChan
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.responses, initResponseID)
		close(initRespChan)
		c.mu.Unlock()
	}()

	// Prepare the initialize message
	params := map[string]string{"sdkLanguage": "python"}
	wallTime := time.Now().UnixMilli()
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "Connection.init",
		"internal": false,
	}

	// Send the initialize message using sendMessage and get immediate response
	resp, err := c.sendMessage(ctx, "", "initialize", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to send initialize message: %w", err)
	}

	c.logger.Debug("Sent initialize message", zap.Int64("wallTime", wallTime))

	// Check if the direct response contains the Playwright initialization
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err == nil {
		if result.Playwright.GUID == "Playwright" {
			c.logger.Debug("Successfully received Playwright initialize response via direct response",
				zap.String("guid", result.Playwright.GUID))

			// Listen for browser creation events for a short time
			eventCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			go func() {
				for {
					select {
					case resp := <-initRespChan:
						if resp.ID == -999 {
							// This is an event
							var event PlaywrightEvent
							if err := json.Unmarshal(resp.Result, &event); err == nil {
								if event.Method == "__create__" && event.Params != nil {
									if eventType, ok := event.Params["type"].(string); ok && eventType == "Browser" {
										c.logger.Debug("Found Browser creation event")
										// Extract browser information
										browserInfo := &BrowserInfo{
											GUID: event.Params["guid"].(string),
											Type: eventType,
										}
										if initializer, ok := event.Params["initializer"].(map[string]interface{}); ok {
											browserInfo.Initializer = initializer
											if name, ok := initializer["name"].(string); ok {
												browserInfo.Name = name
											}
											if version, ok := initializer["version"].(string); ok {
												browserInfo.Version = version
											}
										}
										result.BrowserInfo = browserInfo
										c.logger.Debug("Captured Browser info",
											zap.String("guid", browserInfo.GUID),
											zap.String("name", browserInfo.Name),
											zap.String("version", browserInfo.Version))
									}
								}
							}
						}
					case <-eventCtx.Done():
						return
					}
				}
			}()

			// Wait a bit for browser events
			<-eventCtx.Done()

			return &result, nil
		}
	}

	return nil, fmt.Errorf("failed to parse initialize response")
}

// Initialize method state to track browser info
type initializeState struct {
	result      *InitializeResult
	browserInfo *BrowserInfo
}

// checkInitializeResponse checks if a response contains the expected Playwright initialization result
func (c *PlaywrightClient) checkInitializeResponse(resp PlaywrightResponse, messageCount int, state *initializeState) *InitializeResult {
	c.logger.Debug("Received response during initialize",
		zap.Int("messageCount", messageCount),
		zap.Int("responseId", resp.ID),
		zap.String("result", string(resp.Result)),
		zap.Any("error", resp.Error))

	if resp.Error != nil {
		c.logger.Debug("Received error response", zap.Any("error", resp.Error))
		if resp.Error.Message == "" && resp.Error.Code == 0 {
			return nil // Skip empty errors
		}
		return nil
	}

	// Check if this is an event message (ID -999)
	if resp.ID == -999 {
		// This is an event - try to parse as PlaywrightEvent
		var event PlaywrightEvent
		if err := json.Unmarshal(resp.Result, &event); err == nil {
			c.logger.Debug("Received event during initialize",
				zap.String("method", event.Method),
				zap.String("guid", event.GUID),
				zap.Any("params", event.Params))

			// Check if this is a Browser creation event
			if event.Method == "__create__" && event.Params != nil {
				if eventType, ok := event.Params["type"].(string); ok && eventType == "Browser" {
					c.logger.Debug("Found Browser creation event")

					// Extract browser information
					browserInfo := &BrowserInfo{
						GUID: event.Params["guid"].(string),
						Type: eventType,
					}

					// Extract initializer info if present
					if initializer, ok := event.Params["initializer"].(map[string]interface{}); ok {
						browserInfo.Initializer = initializer
						if name, ok := initializer["name"].(string); ok {
							browserInfo.Name = name
						}
						if version, ok := initializer["version"].(string); ok {
							browserInfo.Version = version
						}
					}

					state.browserInfo = browserInfo
					c.logger.Debug("Captured Browser info",
						zap.String("guid", browserInfo.GUID),
						zap.String("name", browserInfo.Name),
						zap.String("version", browserInfo.Version))
				}
			}
		}
		return nil // Events don't complete initialization
	}

	// Try to parse the result as InitializeResult
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err == nil {
		// Check if this is the expected Playwright initialize response
		if result.Playwright.GUID == "Playwright" {
			// Add captured browser info if we have it
			if state.browserInfo != nil {
				result.BrowserInfo = state.browserInfo
			}

			c.logger.Debug("Successfully received Playwright initialize response",
				zap.String("guid", result.Playwright.GUID),
				zap.Int("responseId", resp.ID),
				zap.Int("totalMessages", messageCount),
				zap.Any("browserInfo", result.BrowserInfo))
			return &result
		}
	}

	return nil
}

// NewBrowserCDPSession creates a new CDP session for the given browser GUID
func (c *PlaywrightClient) NewBrowserCDPSession(ctx context.Context, browserGUID string) (*NewBrowserCDPSessionResult, error) {
	// Prepare metadata with the required apiName
	wallTime := time.Now().UnixMilli()
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "Browser.new_browser_cdp_session",
		"internal": false,
	}

	// Send the newBrowserCDPSession command
	resp, err := c.sendMessage(ctx, browserGUID, "newBrowserCDPSession", nil, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to create new browser CDP session: %w", err)
	}

	var result NewBrowserCDPSessionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal newBrowserCDPSession result: %w", err)
	}

	c.logger.Debug("Successfully created new browser CDP session",
		zap.String("browserGUID", browserGUID),
		zap.String("sessionGUID", result.Session.GUID),
		zap.Int64("wallTime", wallTime))

	return &result, nil
}

// sendCDPMessage sends a CDP message through the session and waits for a response
func (c *PlaywrightClient) sendCDPMessage(ctx context.Context, sessionGUID string, cdpMethod string, cdpParams interface{}, metadata interface{}) (*PlaywrightResponse, error) {
	// Prepare wall time for metadata
	wallTime := time.Now().UnixMilli()

	// Create the wrapped params for the CDP message
	wrappedParams := map[string]interface{}{
		"method": cdpMethod,
		"params": cdpParams,
	}

	// Prepare metadata if not provided
	if metadata == nil {
		metadata = map[string]interface{}{
			"wallTime": wallTime,
			"apiName":  "CDPSession.send",
			"internal": false,
		}
	}

	// Send the command using sendMessage method with "send" as the method and wait for response
	resp, err := c.sendMessage(ctx, sessionGUID, "send", wrappedParams, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to send CDP message %s: %w", cdpMethod, err)
	}

	c.logger.Debug("Successfully sent CDP message and received response",
		zap.String("sessionGUID", sessionGUID),
		zap.String("cdpMethod", cdpMethod),
		zap.Int64("wallTime", wallTime),
		zap.String("response", string(resp.Result)))

	return resp, nil
}

// GetTargetsViaCDP calls Target.getTargets using the CDP session and returns the response
func (c *PlaywrightClient) GetTargetsViaCDP(ctx context.Context, sessionGUID string) (*PlaywrightResponse, error) {
	// Prepare wall time for metadata
	wallTime := time.Now().UnixMilli()

	// Prepare metadata for the CDP command
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	// Prepare parameters for Target.getTargets with default filter
	params := map[string]interface{}{
		"filter": []interface{}{map[string]interface{}{}},
	}

	// Send Target.getTargets command via CDP session
	resp, err := c.sendCDPMessage(ctx, sessionGUID, "Target.getTargets", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to send Target.getTargets via CDP: %w", err)
	}

	c.logger.Debug("Successfully sent Target.getTargets via CDP and received response",
		zap.String("sessionGUID", sessionGUID),
		zap.Int64("wallTime", wallTime),
		zap.String("response", string(resp.Result)))

	return resp, nil
}

// AttachToTargetResult represents the result of Target.attachToTarget command
type AttachToTargetResult struct {
	SessionID string `json:"sessionId"`
}

// AttachToTarget attaches to a target using CDP and returns the session ID
func (c *PlaywrightClient) AttachToTarget(ctx context.Context, sessionGUID string, targetID string) (*AttachToTargetResult, error) {
	// Prepare wall time for metadata
	wallTime := time.Now().UnixMilli()

	// Prepare metadata for the CDP command
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	// Prepare parameters for Target.attachToTarget
	params := map[string]interface{}{
		"targetId": targetID,
		"flatten":  true,
	}

	// Send Target.attachToTarget command via CDP session
	resp, err := c.sendCDPMessage(ctx, sessionGUID, "Target.attachToTarget", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to attach to target %s: %w", targetID, err)
	}

	// Parse the response to extract the session ID
	var result struct {
		Result AttachToTargetResult `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Target.attachToTarget response: %w", err)
	}

	c.logger.Debug("Successfully attached to target",
		zap.String("targetId", targetID),
		zap.String("sessionId", result.Result.SessionID),
		zap.String("sessionGUID", sessionGUID),
		zap.Int64("wallTime", wallTime))

	return &result.Result, nil
}

// PerformanceMetric represents a single performance metric
type PerformanceMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// PerformanceMetricsResult represents the result of Performance.getMetrics command
type PerformanceMetricsResult struct {
	Metrics []PerformanceMetric `json:"metrics"`
}

// GetPerformanceMetrics gets performance metrics from a target session using CDP
func (c *PlaywrightClient) GetPerformanceMetrics(ctx context.Context, targetSessionID string) (*PerformanceMetricsResult, error) {
	// Prepare wall time for metadata
	wallTime := time.Now().UnixMilli()

	// Prepare metadata for the CDP command
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	// Performance.getMetrics doesn't require parameters
	params := map[string]interface{}{}

	// Send Performance.getMetrics command via target's CDP session
	resp, err := c.sendCDPMessage(ctx, targetSessionID, "Memory.getAllTimeSamplingProfile", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to get performance metrics from session %s: %w", targetSessionID, err)
	}

	// Parse the response to extract the performance metrics
	var result struct {
		Result PerformanceMetricsResult `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Performance.getMetrics response: %w", err)
	}

	c.logger.Debug("Successfully retrieved performance metrics",
		zap.String("targetSessionId", targetSessionID),
		zap.Int("metricsCount", len(result.Result.Metrics)),
		zap.Int64("wallTime", wallTime))

	return &result.Result, nil
}
