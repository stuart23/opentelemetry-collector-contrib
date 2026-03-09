// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver"

import (
	"context"
	"errors"
	"time"

	"github.com/chromedp/cdproto/target"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

var (
	errClientNotInit = errors.New("CDP client not initialized")

	intGaugeMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val int64, endpoint, targetType, targetURL string){
		"Documents":        (*metadata.MetricsBuilder).RecordChromiumPageDocumentCountDataPoint,
		"Frames":           (*metadata.MetricsBuilder).RecordChromiumPageFrameCountDataPoint,
		"JSEventListeners": (*metadata.MetricsBuilder).RecordChromiumPageJsEventListenerCountDataPoint,
		"Nodes":            (*metadata.MetricsBuilder).RecordChromiumPageDomNodeCountDataPoint,
		"JSHeapUsedSize":   (*metadata.MetricsBuilder).RecordChromiumPageJsHeapUsedSizeDataPoint,
		"JSHeapTotalSize":  (*metadata.MetricsBuilder).RecordChromiumPageJsHeapTotalSizeDataPoint,
	}

	intSumMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val int64, endpoint, targetType, targetURL string){
		"LayoutCount":      (*metadata.MetricsBuilder).RecordChromiumPageLayoutCountDataPoint,
		"RecalcStyleCount": (*metadata.MetricsBuilder).RecordChromiumPageRecalcStyleCountDataPoint,
	}

	doubleSumMetrics = map[string]func(mb *metadata.MetricsBuilder, ts pcommon.Timestamp, val float64, endpoint, targetType, targetURL string){
		"LayoutDuration":      (*metadata.MetricsBuilder).RecordChromiumPageLayoutDurationDataPoint,
		"RecalcStyleDuration": (*metadata.MetricsBuilder).RecordChromiumPageRecalcStyleDurationDataPoint,
		"ScriptDuration":      (*metadata.MetricsBuilder).RecordChromiumPageScriptDurationDataPoint,
		"TaskDuration":        (*metadata.MetricsBuilder).RecordChromiumPageTaskDurationDataPoint,
	}
)

type chromiumScraper struct {
	client   *CDPClient
	cfg      *Config
	settings component.TelemetrySettings
	mb       *metadata.MetricsBuilder
}

func newScraper(conf *Config, settings receiver.Settings) *chromiumScraper {
	return &chromiumScraper{
		cfg:      conf,
		settings: settings.TelemetrySettings,
		mb:       metadata.NewMetricsBuilder(conf.MetricsBuilderConfig, settings),
	}
}

func (p *chromiumScraper) start(ctx context.Context, _ component.Host) error {
	p.client = NewCDPClient(p.cfg.Endpoint, p.settings.Logger)

	if err := p.client.Connect(ctx); err != nil {
		p.settings.Logger.Error("Failed to connect to CDP endpoint", zap.Error(err))
		return err
	}

	p.settings.Logger.Info("Connected to Chrome via CDP", zap.String("endpoint", p.cfg.Endpoint))
	return nil
}

func (p *chromiumScraper) shutdown(_ context.Context) error {
	if p.client != nil {
		return p.client.Disconnect()
	}
	return nil
}

func (p *chromiumScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	if p.client == nil {
		return pmetric.NewMetrics(), errClientNotInit
	}

	now := pcommon.NewTimestampFromTime(time.Now())

	targets, err := p.client.GetTargets(ctx)
	if err != nil {
		p.settings.Logger.Error("Failed to get targets", zap.Error(err))
	} else {
		p.scrapeTargetCounts(now, targets)
		p.scrapePagePerformanceMetrics(ctx, now, targets)
	}

	return p.mb.Emit(), nil
}

func (p *chromiumScraper) scrapeTargetCounts(now pcommon.Timestamp, targets []TargetInfo) {
	targetCounts := make(map[string]int64)
	for _, t := range targets {
		targetCounts[t.Type]++
	}

	for targetType, count := range targetCounts {
		p.mb.RecordChromiumTargetsCountDataPoint(now, count, p.cfg.Endpoint, targetType)
	}

	p.settings.Logger.Debug("Recorded target counts",
		zap.Int("total_targets", len(targets)),
		zap.Any("counts_by_type", targetCounts))
}

func (p *chromiumScraper) scrapePagePerformanceMetrics(ctx context.Context, now pcommon.Timestamp, targets []TargetInfo) {
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}

		perfCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		perfMetrics, err := p.client.GetPerformanceMetrics(perfCtx, target.ID(t.TargetID))
		cancel()

		if err != nil {
			p.settings.Logger.Warn("Failed to get performance metrics",
				zap.String("targetID", t.TargetID),
				zap.String("url", t.URL),
				zap.Error(err))
			continue
		}

		p.recordPerformanceMetrics(now, perfMetrics, t.URL)

		p.settings.Logger.Debug("Recorded performance metrics for page",
			zap.String("url", t.URL),
			zap.Int("metricsCount", len(perfMetrics.Metrics)))
	}
}

func (p *chromiumScraper) recordPerformanceMetrics(ts pcommon.Timestamp, perfMetrics *PerformanceMetricsResult, targetURL string) {
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
