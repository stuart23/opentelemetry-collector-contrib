// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver"

import (
	"errors"
	"fmt"
	"net/url"

	"go.opentelemetry.io/collector/scraper/scraperhelper"
	"go.uber.org/multierr"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

var (
	errInvalidEndpoint = errors.New(`"endpoint" must be a valid Chrome DevTools Protocol WebSocket URL`)
	errMissingEndpoint = errors.New(`"endpoint" must be specified`)
)

// Config defines the configuration for the Chromium receiver.
type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`
	metadata.MetricsBuilderConfig  `mapstructure:",squash"`

	// Endpoint is the Chrome DevTools Protocol WebSocket debugger URL
	// (e.g., ws://localhost:9222). Start Chrome with
	// --remote-debugging-port=9222 to expose this endpoint.
	Endpoint string `mapstructure:"endpoint"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// Validate validates the configuration.
func (cfg *Config) Validate() error {
	var err error

	if cfg.Endpoint == "" {
		err = multierr.Append(err, errMissingEndpoint)
	} else {
		if u, parseErr := url.Parse(cfg.Endpoint); parseErr != nil {
			err = multierr.Append(err, fmt.Errorf("%s: %w", errInvalidEndpoint.Error(), parseErr))
		} else if u.Scheme != "ws" && u.Scheme != "wss" {
			err = multierr.Append(err, errors.New(`"endpoint" must use ws:// or wss:// scheme`))
		}
	}

	return err
}
