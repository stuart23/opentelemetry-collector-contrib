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

// PageInfo tracks a Playwright page object
type PageInfo struct {
	GUID        string
	ContextGUID string
	FrameGUID   string
	URL         string
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

	// Tracked Playwright objects from __create__ events
	pagesMu  sync.RWMutex
	pages    map[string]*PageInfo // pageGUID → PageInfo
	contexts map[string]string    // contextGUID → browserGUID
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
		pages:         make(map[string]*PageInfo),
		contexts:      make(map[string]string),
	}
}

// Connect establishes a WebSocket connection to the Playwright endpoint
func (c *PlaywrightClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Playwright endpoint %s: %w", c.endpoint, err)
	}

	c.conn = conn
	c.closed = false

	go c.readResponses()
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

	for _, ch := range c.responses {
		close(ch)
	}
	c.responses = make(map[int]chan PlaywrightResponse)

	err := c.conn.Close()
	c.conn = nil
	return err
}

// Pages returns a snapshot of all tracked page objects
func (c *PlaywrightClient) Pages() []*PageInfo {
	c.pagesMu.RLock()
	defer c.pagesMu.RUnlock()

	pages := make([]*PageInfo, 0, len(c.pages))
	for _, p := range c.pages {
		pages = append(pages, p)
	}
	return pages
}

func (c *PlaywrightClient) handleReconnection() {
	for {
		select {
		case <-c.reconnectChan:
			c.logger.Info("Starting automatic reconnection...")
			time.Sleep(2 * time.Second)

			if err := c.reconnectInternal(); err != nil {
				c.logger.Error("Failed to reconnect", zap.Error(err))
				select {
				case c.reconnectChan <- struct{}{}:
				default:
				}
			} else {
				c.logger.Info("Successfully reconnected to Playwright server")

				c.mu.Lock()
				callback := c.reconnectCallback
				c.mu.Unlock()

				if callback != nil {
					if err := callback(); err != nil {
						c.logger.Error("Reconnect callback failed", zap.Error(err))
					}
				}
			}

		case <-c.closeChan:
			return
		}
	}
}

func (c *PlaywrightClient) reconnectInternal() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	if c.closed {
		return fmt.Errorf("client is closed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to reconnect to Playwright endpoint %s: %w", c.endpoint, err)
	}

	c.conn = conn
	go c.readResponses()

	return nil
}

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

	var paramsMap map[string]interface{}
	if params != nil {
		if pm, ok := params.(map[string]interface{}); ok {
			paramsMap = pm
		} else if ps, ok := params.(map[string]string); ok {
			paramsMap = make(map[string]interface{})
			for k, v := range ps {
				paramsMap[k] = v
			}
		} else {
			return nil, fmt.Errorf("params must be map[string]interface{} or map[string]string")
		}
	}

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

	c.mu.Lock()
	err := c.conn.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to send Playwright message: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != nil && (resp.Error.Code != 0 || resp.Error.Message != "") {
			return nil, fmt.Errorf("Playwright error: %s (code: %d)", resp.Error.Message, resp.Error.Code)
		}
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeChan:
		return nil, fmt.Errorf("connection closed")
	}
}

// handleCreateEvent processes __create__ events to track pages and contexts
func (c *PlaywrightClient) handleCreateEvent(event PlaywrightEvent) {
	if event.Method != "__create__" || event.Params == nil {
		return
	}

	objType, _ := event.Params["type"].(string)
	objGUID, _ := event.Params["guid"].(string)
	if objGUID == "" {
		return
	}

	switch objType {
	case "BrowserContext":
		c.pagesMu.Lock()
		c.contexts[objGUID] = event.GUID
		c.pagesMu.Unlock()
		c.logger.Debug("Tracked BrowserContext", zap.String("guid", objGUID))

	case "Page":
		info := &PageInfo{
			GUID:        objGUID,
			ContextGUID: event.GUID,
		}
		if initializer, ok := event.Params["initializer"].(map[string]interface{}); ok {
			if mainFrame, ok := initializer["mainFrame"].(map[string]interface{}); ok {
				if url, ok := mainFrame["url"].(string); ok {
					info.URL = url
				}
				if guid, ok := mainFrame["guid"].(string); ok {
					info.FrameGUID = guid
				}
			}
		}
		c.pagesMu.Lock()
		c.pages[objGUID] = info
		c.pagesMu.Unlock()
		c.logger.Debug("Tracked Page", zap.String("guid", objGUID),
			zap.String("url", info.URL), zap.String("frameGUID", info.FrameGUID))
	}
}

// handleDestroyEvent processes __dispose__ events to remove tracked objects
func (c *PlaywrightClient) handleDestroyEvent(event PlaywrightEvent) {
	if event.Method != "__dispose__" {
		return
	}

	c.pagesMu.Lock()
	defer c.pagesMu.Unlock()
	delete(c.pages, event.GUID)
	delete(c.contexts, event.GUID)
}

func (c *PlaywrightClient) readResponses() {
	for {
		c.mu.Lock()
		if c.conn == nil || c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		_, rawMessage, err := c.conn.ReadMessage()
		if err != nil {
			c.logger.Warn("WebSocket connection lost", zap.Error(err))

			c.mu.Lock()
			if c.autoReconnect && !c.closed {
				select {
				case c.reconnectChan <- struct{}{}:
				default:
				}
			}
			c.mu.Unlock()
			return
		}

		var resp PlaywrightResponse
		if err := json.Unmarshal(rawMessage, &resp); err == nil && (resp.ID != 0 || len(resp.Result) > 0 || resp.Error != nil) {
			c.mu.Lock()
			if respChan, exists := c.responses[resp.ID]; exists {
				select {
				case respChan <- resp:
				default:
				}
			}
			if len(resp.Result) > 0 {
				if initChan, exists := c.responses[-1]; exists {
					select {
					case initChan <- resp:
					default:
					}
				}
			}
			c.mu.Unlock()
		} else {
			var event PlaywrightEvent
			if err := json.Unmarshal(rawMessage, &event); err == nil && event.Method != "" {
				// Track pages/contexts from create/dispose events
				c.handleCreateEvent(event)
				c.handleDestroyEvent(event)

				c.mu.Lock()
				if initChan, exists := c.responses[-1]; exists {
					eventResp := PlaywrightResponse{
						ID:     -999,
						Result: rawMessage,
					}
					select {
					case initChan <- eventResp:
					default:
					}
				}
				c.mu.Unlock()
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
	initRespChan := make(chan PlaywrightResponse, 50)

	c.mu.Lock()
	initResponseID := -1
	c.responses[initResponseID] = initRespChan
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.responses, initResponseID)
		close(initRespChan)
		c.mu.Unlock()
	}()

	params := map[string]string{"sdkLanguage": "python"}
	wallTime := time.Now().UnixMilli()
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "Connection.init",
		"internal": false,
	}

	resp, err := c.sendMessage(ctx, "", "initialize", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to send initialize message: %w", err)
	}

	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err == nil {
		if result.Playwright.GUID == "Playwright" {
			c.logger.Debug("Successfully received Playwright initialize response",
				zap.String("guid", result.Playwright.GUID))

			// Wait for browser/context/page creation events
			eventCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			go func() {
				for {
					select {
					case resp := <-initRespChan:
						if resp.ID == -999 {
							var event PlaywrightEvent
							if err := json.Unmarshal(resp.Result, &event); err == nil {
								if event.Method == "__create__" && event.Params != nil {
									if eventType, ok := event.Params["type"].(string); ok && eventType == "Browser" {
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

			<-eventCtx.Done()
			return &result, nil
		}
	}

	return nil, fmt.Errorf("failed to parse initialize response")
}

// NewBrowserCDPSession creates a new CDP session for the given browser GUID
func (c *PlaywrightClient) NewBrowserCDPSession(ctx context.Context, browserGUID string) (*NewBrowserCDPSessionResult, error) {
	wallTime := time.Now().UnixMilli()
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "Browser.new_browser_cdp_session",
		"internal": false,
	}

	resp, err := c.sendMessage(ctx, browserGUID, "newBrowserCDPSession", nil, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to create new browser CDP session: %w", err)
	}

	var result NewBrowserCDPSessionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal newBrowserCDPSession result: %w", err)
	}

	return &result, nil
}

// NewPageCDPSession creates a CDP session for a specific page through its BrowserContext.
// This returns a CDPSession GUID that can be used to send page-level CDP commands.
func (c *PlaywrightClient) NewPageCDPSession(ctx context.Context, contextGUID string, pageGUID string) (*NewBrowserCDPSessionResult, error) {
	wallTime := time.Now().UnixMilli()
	metadata := map[string]interface{}{
		"wallTime": wallTime,
		"apiName":  "BrowserContext.new_cdp_session",
		"internal": false,
	}

	params := map[string]interface{}{
		"page": map[string]interface{}{
			"guid": pageGUID,
		},
	}

	resp, err := c.sendMessage(ctx, contextGUID, "newCDPSession", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to create page CDP session for %s: %w", pageGUID, err)
	}

	var result NewBrowserCDPSessionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal newCDPSession result: %w", err)
	}

	return &result, nil
}

// sendCDPMessage sends a CDP message through the session and waits for a response
func (c *PlaywrightClient) sendCDPMessage(ctx context.Context, sessionGUID string, cdpMethod string, cdpParams interface{}, metadata interface{}) (*PlaywrightResponse, error) {
	wallTime := time.Now().UnixMilli()

	wrappedParams := map[string]interface{}{
		"method": cdpMethod,
		"params": cdpParams,
	}

	if metadata == nil {
		metadata = map[string]interface{}{
			"wallTime": wallTime,
			"apiName":  "CDPSession.send",
			"internal": false,
		}
	}

	resp, err := c.sendMessage(ctx, sessionGUID, "send", wrappedParams, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to send CDP message %s: %w", cdpMethod, err)
	}

	return resp, nil
}

// GetTargetsViaCDP calls Target.getTargets using the CDP session and returns the response
func (c *PlaywrightClient) GetTargetsViaCDP(ctx context.Context, sessionGUID string) (*PlaywrightResponse, error) {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	params := map[string]interface{}{
		"filter": []interface{}{map[string]interface{}{}},
	}

	return c.sendCDPMessage(ctx, sessionGUID, "Target.getTargets", params, metadata)
}

// AttachToTargetResult represents the result of Target.attachToTarget command
type AttachToTargetResult struct {
	SessionID string `json:"sessionId"`
}

// AttachToTarget attaches to a target using CDP and returns the session ID
func (c *PlaywrightClient) AttachToTarget(ctx context.Context, sessionGUID string, targetID string) (*AttachToTargetResult, error) {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	params := map[string]interface{}{
		"targetId": targetID,
		"flatten":  true,
	}

	resp, err := c.sendCDPMessage(ctx, sessionGUID, "Target.attachToTarget", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to attach to target %s: %w", targetID, err)
	}

	var result struct {
		Result AttachToTargetResult `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Target.attachToTarget response: %w", err)
	}

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

// EnablePerformance enables the Performance CDP domain on a session.
// Must be called before GetPerformanceMetrics on some Chrome versions.
func (c *PlaywrightClient) EnablePerformance(ctx context.Context, sessionGUID string) (*PlaywrightResponse, error) {
	return c.sendCDPMessage(ctx, sessionGUID, "Performance.enable", map[string]interface{}{}, nil)
}

// GetPerformanceMetrics gets performance metrics from a CDP session (browser or page level)
func (c *PlaywrightClient) GetPerformanceMetrics(ctx context.Context, sessionGUID string) (*PerformanceMetricsResult, error) {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "CDPSession.send",
		"internal": false,
	}

	params := map[string]interface{}{}

	resp, err := c.sendCDPMessage(ctx, sessionGUID, "Performance.getMetrics", params, metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to get performance metrics from session %s: %w", sessionGUID, err)
	}

	var result struct {
		Result PerformanceMetricsResult `json:"result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Performance.getMetrics response: %w", err)
	}

	return &result.Result, nil
}

// NewBrowserContext creates a new BrowserContext through the Playwright protocol.
// Returns the context GUID.
func (c *PlaywrightClient) NewBrowserContext(ctx context.Context, browserGUID string) (string, error) {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "Browser.new_context",
		"internal": false,
	}

	params := map[string]interface{}{
		"noDefaultViewport": false,
	}

	resp, err := c.sendMessage(ctx, browserGUID, "newContext", params, metadata)
	if err != nil {
		return "", fmt.Errorf("failed to create browser context: %w", err)
	}

	var result struct {
		Context struct {
			GUID string `json:"guid"`
		} `json:"context"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("failed to parse newContext result: %w", err)
	}

	c.logger.Debug("Created BrowserContext", zap.String("guid", result.Context.GUID))
	return result.Context.GUID, nil
}

// NewPage creates a new Page in the given BrowserContext through the Playwright protocol.
// The returned page GUID can be used with NewPageCDPSession.
// The corresponding __create__ event is handled by readResponses to track the page.
func (c *PlaywrightClient) NewPage(ctx context.Context, contextGUID string) (string, error) {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "BrowserContext.new_page",
		"internal": false,
	}

	resp, err := c.sendMessage(ctx, contextGUID, "newPage", nil, metadata)
	if err != nil {
		return "", fmt.Errorf("failed to create page: %w", err)
	}

	var result struct {
		Page struct {
			GUID string `json:"guid"`
		} `json:"page"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("failed to parse newPage result: %w", err)
	}

	c.logger.Debug("Created Page", zap.String("guid", result.Page.GUID))
	return result.Page.GUID, nil
}

// NavigatePage navigates a page's main frame to the given URL.
func (c *PlaywrightClient) NavigatePage(ctx context.Context, pageGUID string, url string) error {
	metadata := map[string]interface{}{
		"wallTime": time.Now().UnixMilli(),
		"apiName":  "Frame.goto",
		"internal": false,
	}

	// Find the main frame GUID for this page
	c.pagesMu.RLock()
	page, exists := c.pages[pageGUID]
	c.pagesMu.RUnlock()

	if !exists {
		// Wait briefly for __create__ events to arrive
		time.Sleep(200 * time.Millisecond)
		c.pagesMu.RLock()
		page, exists = c.pages[pageGUID]
		c.pagesMu.RUnlock()
	}

	// Use the page's frame GUID if available, otherwise try sending to the page directly
	targetGUID := pageGUID
	if exists && page.FrameGUID != "" {
		targetGUID = page.FrameGUID
	}

	params := map[string]interface{}{
		"url":       url,
		"waitUntil": "load",
	}

	_, err := c.sendMessage(ctx, targetGUID, "goto", params, metadata)
	if err != nil {
		return err
	}

	c.pagesMu.Lock()
	if p, ok := c.pages[pageGUID]; ok {
		p.URL = url
	}
	c.pagesMu.Unlock()

	return nil
}
