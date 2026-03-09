// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package playwrightreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	intGaugeMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val int64, endpoint, targetType, targetURL string){
		"Documents":        (*metadata.MetricsBuilder).RecordPlaywrightPageDocumentCountDataPoint,
		"Frames":           (*metadata.MetricsBuilder).RecordPlaywrightPageFrameCountDataPoint,
		"JSEventListeners": (*metadata.MetricsBuilder).RecordPlaywrightPageJsEventListenerCountDataPoint,
		"Nodes":            (*metadata.MetricsBuilder).RecordPlaywrightPageDomNodeCountDataPoint,
		"JSHeapUsedSize":   (*metadata.MetricsBuilder).RecordPlaywrightPageJsHeapUsedSizeDataPoint,
		"JSHeapTotalSize":  (*metadata.MetricsBuilder).RecordPlaywrightPageJsHeapTotalSizeDataPoint,
	}

	intSumMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val int64, endpoint, targetType, targetURL string){
		"LayoutCount":      (*metadata.MetricsBuilder).RecordPlaywrightPageLayoutCountDataPoint,
		"RecalcStyleCount": (*metadata.MetricsBuilder).RecordPlaywrightPageRecalcStyleCountDataPoint,
	}

	doubleSumMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val float64, endpoint, targetType, targetURL string){
		"LayoutDuration":      (*metadata.MetricsBuilder).RecordPlaywrightPageLayoutDurationDataPoint,
		"RecalcStyleDuration": (*metadata.MetricsBuilder).RecordPlaywrightPageRecalcStyleDurationDataPoint,
		"ScriptDuration":     (*metadata.MetricsBuilder).RecordPlaywrightPageScriptDurationDataPoint,
		"TaskDuration":       (*metadata.MetricsBuilder).RecordPlaywrightPageTaskDurationDataPoint,
	}
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
	sessionGUID string // browser-level CDP session for Target.getTargets
	browserGUID string
}

func newScraper(conf *Config, settings receiver.Settings) *playwrightScraper {
	return &playwrightScraper{
		cfg:      conf,
		settings: settings.TelemetrySettings,
		mb:       metadata.NewMetricsBuilder(conf.MetricsBuilderConfig, settings),
	}
}

func (p *playwrightScraper) start(ctx context.Context, host component.Host) error {
	p.client = NewPlaywrightClient(p.cfg.Endpoint, p.settings.Logger)

	p.client.SetReconnectCallback(func() error {
		return p.initializePlaywrightSession(context.Background())
	})

	if err := p.client.Connect(ctx); err != nil {
		p.settings.Logger.Error("Failed to connect to Playwright endpoint", zap.Error(err))
		return err
	}

	p.settings.Logger.Info("Successfully connected to Playwright endpoint", zap.String("endpoint", p.cfg.Endpoint))
	return p.initializePlaywrightSession(ctx)
}

func (p *playwrightScraper) initializePlaywrightSession(ctx context.Context) error {
	p.settings.Logger.Info("Initializing Playwright session...")

	initResult, err := p.client.Initialize(ctx)
	if err != nil {
		p.settings.Logger.Error("Failed to initialize Playwright", zap.Error(err))
		return err
	}

	p.settings.Logger.Info("Successfully initialized Playwright",
		zap.String("playwrightGUID", initResult.Playwright.GUID))

	if initResult.BrowserInfo == nil {
		p.settings.Logger.Warn("No browser information available from initialization")
		p.sessionGUID = ""
		return nil
	}

	p.browserGUID = initResult.BrowserInfo.GUID
	p.settings.Logger.Info("Browser information captured",
		zap.String("browserGUID", p.browserGUID),
		zap.String("browserName", initResult.BrowserInfo.Name),
		zap.String("browserVersion", initResult.BrowserInfo.Version))

	sessionResult, err := p.client.NewBrowserCDPSession(ctx, p.browserGUID)
	if err != nil {
		p.settings.Logger.Error("Failed to create CDP session", zap.Error(err))
		return err
	}

	p.sessionGUID = sessionResult.Session.GUID
	p.settings.Logger.Info("Successfully created browser CDP session",
		zap.String("sessionGUID", p.sessionGUID))

	// Create a monitoring page so we can get page-level CDP sessions.
	// The Playwright protocol isolates pages by connection, so we need
	// our own page to access Performance.getMetrics via CDP.
	if err := p.createMonitoringPage(ctx); err != nil {
		p.settings.Logger.Warn("Could not create monitoring page; performance metrics will be unavailable",
			zap.Error(err))
	}

	return nil
}

// createMonitoringPage creates a BrowserContext and Page owned by this client
// so we can create page-level CDP sessions for Performance.getMetrics.
func (p *playwrightScraper) createMonitoringPage(ctx context.Context) error {
	contextGUID, err := p.client.NewBrowserContext(ctx, p.browserGUID)
	if err != nil {
		return fmt.Errorf("failed to create browser context: %w", err)
	}

	pageGUID, err := p.client.NewPage(ctx, contextGUID)
	if err != nil {
		return fmt.Errorf("failed to create page: %w", err)
	}

	// Allow __create__ events to propagate so the page is tracked
	time.Sleep(200 * time.Millisecond)

	p.settings.Logger.Info("Created monitoring page",
		zap.String("contextGUID", contextGUID),
		zap.String("pageGUID", pageGUID))

	return nil
}

func (p *playwrightScraper) shutdown(ctx context.Context) error {
	if p.client != nil {
		return p.client.Disconnect()
	}
	return nil
}

func (p *playwrightScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	if p.client == nil {
		return pmetric.NewMetrics(), errClientNotInit
	}

	now := pcommon.NewTimestampFromTime(time.Now())

	// Collect target counts via browser CDP session
	if p.sessionGUID != "" {
		p.scrapeTargetCounts(ctx, now)
	}

	// Collect performance metrics via page-level CDP sessions
	p.scrapePagePerformanceMetrics(ctx, now)

	return p.mb.Emit(), nil
}

// scrapeTargetCounts uses the browser-level CDP session to count targets by type
func (p *playwrightScraper) scrapeTargetCounts(ctx context.Context, now pcommon.Timestamp) {
	resp, err := p.client.GetTargetsViaCDP(ctx, p.sessionGUID)
	if err != nil {
		p.settings.Logger.Error("Failed to get targets via CDP", zap.Error(err))
		return
	}

	var targetsResp TargetsResponse
	if err := json.Unmarshal(resp.Result, &targetsResp); err != nil {
		p.settings.Logger.Error("Failed to parse targets response", zap.Error(err))
		return
	}

	targetCounts := make(map[string]int64)
	for _, target := range targetsResp.Result.TargetInfos {
		targetCounts[target.Type]++
	}

	for targetType, count := range targetCounts {
		p.mb.RecordPlaywrightTargetsCountDataPoint(now, count, p.cfg.Endpoint, targetType)
	}

	p.settings.Logger.Debug("Recorded target counts",
		zap.Int("total_targets", len(targetsResp.Result.TargetInfos)),
		zap.Any("counts_by_type", targetCounts))
}

// scrapePagePerformanceMetrics creates page-level CDP sessions to get Performance.getMetrics
func (p *playwrightScraper) scrapePagePerformanceMetrics(ctx context.Context, now pcommon.Timestamp) {
	pages := p.client.Pages()
	if len(pages) == 0 {
		p.settings.Logger.Debug("No tracked pages for performance metrics")
		return
	}

	for _, page := range pages {
		perfCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

		sessionResult, err := p.client.NewPageCDPSession(perfCtx, page.ContextGUID, page.GUID)
		if err != nil {
			cancel()
			p.settings.Logger.Warn("Failed to create page CDP session",
				zap.String("pageGUID", page.GUID),
				zap.String("url", page.URL),
				zap.Error(err))
			continue
		}

		if _, enableErr := p.client.EnablePerformance(perfCtx, sessionResult.Session.GUID); enableErr != nil {
			cancel()
			p.settings.Logger.Warn("Failed to enable Performance domain",
				zap.String("pageGUID", page.GUID), zap.Error(enableErr))
			continue
		}

		perfMetrics, err := p.client.GetPerformanceMetrics(perfCtx, sessionResult.Session.GUID)
		cancel()

		if err != nil {
			p.settings.Logger.Warn("Failed to get performance metrics",
				zap.String("pageGUID", page.GUID),
				zap.String("url", page.URL),
				zap.Error(err))
			continue
		}

		p.recordPerformanceMetrics(now, perfMetrics, page.URL)

		p.settings.Logger.Debug("Recorded performance metrics for page",
			zap.String("url", page.URL),
			zap.Int("metricsCount", len(perfMetrics.Metrics)))
	}
}

func (p *playwrightScraper) recordPerformanceMetrics(ts pcommon.Timestamp, perfMetrics *PerformanceMetricsResult, targetURL string) {
	endpoint := p.cfg.Endpoint
	targetType := "page"

	for _, m := range perfMetrics.Metrics {
		if recorder, ok := intGaugeMetrics[m.Name]; ok {
			recorder(p.mb, ts, int64(m.Value), endpoint, targetType, targetURL)
			continue
		}
		if recorder, ok := intSumMetrics[m.Name]; ok {
			recorder(p.mb, ts, int64(m.Value), endpoint, targetType, targetURL)
			continue
		}
		if recorder, ok := doubleSumMetrics[m.Name]; ok {
			recorder(p.mb, ts, m.Value, endpoint, targetType, targetURL)
			continue
		}
	}
}
