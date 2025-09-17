// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package playwrightreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver"

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver/internal/metadata"
)

var (
	errClientNotInit = errors.New("Playwright client not initialized")
)

// TargetsResponse represents the response from Target.getTargets
type TargetsResponse struct {
	Result struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	} `json:"result"`
}

type playwrightScraper struct {
	client      *PlaywrightClient
	cfg         *Config
	settings    component.TelemetrySettings
	mb          *metadata.MetricsBuilder
	sessionGUID string
}

// newScraper creates a new Playwright scraper
func newScraper(conf *Config, settings receiver.Settings) *playwrightScraper {
	return &playwrightScraper{
		cfg:      conf,
		settings: settings.TelemetrySettings,
		mb:       metadata.NewMetricsBuilder(conf.MetricsBuilderConfig, settings),
	}
}

// start initializes the scraper by creating and connecting the Playwright client
func (p *playwrightScraper) start(ctx context.Context, host component.Host) error {
	p.client = NewPlaywrightClient(p.cfg.Endpoint, p.settings.Logger)

	// Set up reconnection callback to re-initialize after reconnection
	p.client.SetReconnectCallback(func() error {
		return p.initializePlaywrightSession(context.Background())
	})

	// Connect to the Playwright endpoint
	if err := p.client.Connect(ctx); err != nil {
		p.settings.Logger.Error("Failed to connect to Playwright endpoint", zap.Error(err))
		return err
	}

	p.settings.Logger.Info("Successfully connected to Playwright endpoint", zap.String("endpoint", p.cfg.Endpoint))

	// Initialize the Playwright session
	return p.initializePlaywrightSession(ctx)
}

// initializePlaywrightSession initializes the Playwright session and creates a CDP session
func (p *playwrightScraper) initializePlaywrightSession(ctx context.Context) error {
	p.settings.Logger.Info("Initializing Playwright session...")

	// Initialize Playwright and capture browser information
	initResult, err := p.client.Initialize(ctx)
	if err != nil {
		p.settings.Logger.Error("Failed to initialize Playwright", zap.Error(err))
		return err
	}

	p.settings.Logger.Info("Successfully initialized Playwright",
		zap.String("playwrightGUID", initResult.Playwright.GUID))

	// Check if we have browser information
	if initResult.BrowserInfo == nil {
		p.settings.Logger.Warn("No browser information available from initialization")
		// Clear session GUID since we don't have a valid session
		p.sessionGUID = ""
		return nil
	}

	p.settings.Logger.Info("Browser information captured",
		zap.String("browserGUID", initResult.BrowserInfo.GUID),
		zap.String("browserName", initResult.BrowserInfo.Name),
		zap.String("browserVersion", initResult.BrowserInfo.Version))

	// Create a new CDP session using the browser GUID
	sessionResult, err := p.client.NewBrowserCDPSession(ctx, initResult.BrowserInfo.GUID)
	if err != nil {
		p.settings.Logger.Error("Failed to create CDP session",
			zap.String("browserGUID", initResult.BrowserInfo.GUID),
			zap.Error(err))
		return err
	}

	// Store the session GUID for later use
	p.sessionGUID = sessionResult.Session.GUID
	p.settings.Logger.Info("Successfully created CDP session",
		zap.String("sessionGUID", p.sessionGUID),
		zap.String("browserGUID", initResult.BrowserInfo.GUID))

	return nil
}

// shutdown cleans up resources
func (p *playwrightScraper) shutdown(ctx context.Context) error {
	if p.client != nil {
		return p.client.Disconnect()
	}
	return nil
}

// scrape performs the metrics collection
func (p *playwrightScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	if p.client == nil {
		return pmetric.NewMetrics(), errClientNotInit
	}

	// Get targets via CDP session if we have a session
	if p.sessionGUID != "" {
		resp, err := p.client.GetTargetsViaCDP(ctx, p.sessionGUID)
		if err != nil {
			p.settings.Logger.Error("Failed to get targets via CDP",
				zap.String("sessionGUID", p.sessionGUID),
				zap.Error(err))
		} else {
			p.settings.Logger.Debug("Successfully received Target.getTargets response via CDP",
				zap.String("sessionGUID", p.sessionGUID),
				zap.String("endpoint", p.cfg.Endpoint),
				zap.String("response", string(resp.Result)))

			// Parse the targets response and record metrics
			err = p.parseTargetsAndRecordMetrics(resp.Result)
			if err != nil {
				p.settings.Logger.Error("Failed to parse targets response",
					zap.Error(err),
					zap.String("response", string(resp.Result)))
			}
		}
	}

	p.settings.Logger.Debug("Scrape completed - target metrics recorded",
		zap.String("endpoint", p.cfg.Endpoint),
		zap.String("sessionGUID", p.sessionGUID))

	return p.mb.Emit(), nil
}

// parseTargetsAndRecordMetrics parses the target response and records metrics for each target type
func (p *playwrightScraper) parseTargetsAndRecordMetrics(responseData []byte) error {
	var targetsResp TargetsResponse
	if err := json.Unmarshal(responseData, &targetsResp); err != nil {
		return err
	}

	// Count targets by type and attach to each target
	targetCounts := make(map[string]int64)
	attachedSessions := make([]string, 0, len(targetsResp.Result.TargetInfos))

	for _, target := range targetsResp.Result.TargetInfos {
		targetCounts[target.Type]++

		// Attach to each target using AttachToTarget
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		attachResult, err := p.client.AttachToTarget(ctx, p.sessionGUID, target.TargetID)
		cancel()

		if err != nil {
			p.settings.Logger.Warn("Failed to attach to target",
				zap.String("targetId", target.TargetID),
				zap.String("targetType", target.Type),
				zap.String("sessionGUID", p.sessionGUID),
				zap.Error(err))
		} else {
			attachedSessions = append(attachedSessions, attachResult.SessionID)
			p.settings.Logger.Debug("Successfully attached to target",
				zap.String("targetId", target.TargetID),
				zap.String("targetType", target.Type),
				zap.String("targetTitle", target.Title),
				zap.String("targetUrl", target.URL),
				zap.String("sessionId", attachResult.SessionID),
				zap.String("sessionGUID", p.sessionGUID))

			// Get performance metrics for the attached target session
			perfCtx, perfCancel := context.WithTimeout(context.Background(), 5*time.Second)
			perfMetrics, perfErr := p.client.GetPerformanceMetrics(perfCtx, attachResult.SessionID)
			perfCancel()

			if perfErr != nil {
				p.settings.Logger.Warn("Failed to get performance metrics from target session",
					zap.String("targetId", target.TargetID),
					zap.String("targetType", target.Type),
					zap.String("sessionId", attachResult.SessionID),
					zap.Error(perfErr))
			} else {
				p.settings.Logger.Info("Successfully retrieved performance metrics from target",
					zap.String("targetId", target.TargetID),
					zap.String("targetType", target.Type),
					zap.String("targetTitle", target.Title),
					zap.String("targetUrl", target.URL),
					zap.String("sessionId", attachResult.SessionID),
					zap.Int("metricsCount", len(perfMetrics.Metrics)))

				// Log individual performance metrics
				for _, metric := range perfMetrics.Metrics {
					p.settings.Logger.Debug("Performance metric",
						zap.String("targetId", target.TargetID),
						zap.String("targetType", target.Type),
						zap.String("sessionId", attachResult.SessionID),
						zap.String("metricName", metric.Name),
						zap.Float64("metricValue", metric.Value))
				}
			}
		}
	}

	// Record metrics for each target type
	now := pcommon.NewTimestampFromTime(time.Now())
	for targetType, count := range targetCounts {
		p.mb.RecordPlaywrightTargetsCountDataPoint(now, count, p.cfg.Endpoint, targetType)
		p.settings.Logger.Debug("Recorded target count metric",
			zap.String("target.type", targetType),
			zap.Int64("count", count),
			zap.String("endpoint", p.cfg.Endpoint))
	}

	p.settings.Logger.Info("Successfully recorded target metrics and attached to targets",
		zap.Int("total_targets", len(targetsResp.Result.TargetInfos)),
		zap.Int("unique_types", len(targetCounts)),
		zap.Int("attached_sessions", len(attachedSessions)),
		zap.Any("counts_by_type", targetCounts),
		zap.Strings("attached_session_ids", attachedSessions))

	return nil
}
