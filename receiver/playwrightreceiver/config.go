// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package playwrightreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver"

import (
	"errors"
	"fmt"
	"net/url"

	"go.opentelemetry.io/collector/scraper/scraperhelper"
	"go.uber.org/multierr"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/playwrightreceiver/internal/metadata"
)

// Predefined error responses for configuration validation failures
var (
	errInvalidEndpoint = errors.New(`"endpoint" must be a valid Playwright WebSocket URL`)
	errMissingEndpoint = errors.New(`"endpoint" must be specified`)
)

// Config defines the configuration for the Playwright receiver.
type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`
	metadata.MetricsBuilderConfig  `mapstructure:",squash"`

	// Endpoint is the Playwright WebSocket endpoint URL to connect to (e.g., ws://localhost:9222)
	Endpoint string `mapstructure:"endpoint"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// Validate validates the configuration.
func (cfg *Config) Validate() error {
	var err error

	// Ensure endpoint is specified
	if cfg.Endpoint == "" {
		err = multierr.Append(err, errMissingEndpoint)
	} else {
		// Validate that endpoint is a valid WebSocket URL
		if u, parseErr := url.Parse(cfg.Endpoint); parseErr != nil {
			err = multierr.Append(err, fmt.Errorf("%s: %w", errInvalidEndpoint.Error(), parseErr))
		} else if u.Scheme != "ws" && u.Scheme != "wss" {
			err = multierr.Append(err, errors.New(`"endpoint" must use ws:// or wss:// scheme`))
		}
	}

	return err
}
