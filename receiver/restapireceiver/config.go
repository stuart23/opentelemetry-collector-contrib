// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package restapireceiver

import (
	"go.opentelemetry.io/collector/config"
)

// Config defines configuration for the REST API receiver.
type Config struct {
	config.ReceiverSettings `mapstructure:",squash"`
	Endpoint                string `mapstructure:"endpoint"`
}
