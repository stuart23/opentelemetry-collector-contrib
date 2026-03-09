// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver"

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

var errConfigNotChromium = errors.New("config was not a Chromium receiver config")

// NewFactory creates a new receiver factory
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, metadata.MetricsStability))
}

func createDefaultConfig() component.Config {
	cfg := scraperhelper.NewDefaultControllerConfig()
	cfg.CollectionInterval = 30 * time.Second

	return &Config{
		ControllerConfig:     cfg,
		MetricsBuilderConfig: metadata.DefaultMetricsBuilderConfig(),
		Endpoint:             "",
	}
}

func createMetricsReceiver(_ context.Context, params receiver.Settings, rConf component.Config, consumer consumer.Metrics) (receiver.Metrics, error) {
	cfg, ok := rConf.(*Config)
	if !ok {
		return nil, errConfigNotChromium
	}

	chromiumScraper := newScraper(cfg, params)
	s, err := scraper.NewMetrics(chromiumScraper.scrape, scraper.WithStart(chromiumScraper.start), scraper.WithShutdown(chromiumScraper.shutdown))
	if err != nil {
		return nil, err
	}

	return scraperhelper.NewMetricsController(&cfg.ControllerConfig, params, consumer, scraperhelper.AddScraper(metadata.Type, s))
}
