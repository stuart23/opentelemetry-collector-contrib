// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/chromiumreceiver/internal/metadata"
)

func TestCreateDefaultConfig(t *testing.T) {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig()
	assert.NotNil(t, cfg, "failed to create default config")
	assert.NoError(t, componenttest.CheckConfigStruct(cfg))

	chromiumCfg, ok := cfg.(*Config)
	require.True(t, ok)
	assert.Equal(t, "", chromiumCfg.Endpoint)
	assert.Equal(t, 30*time.Second, chromiumCfg.ControllerConfig.CollectionInterval)
}

func TestCreateMetricsReceiver(t *testing.T) {
	factory := NewFactory()
	cfg := &Config{
		Endpoint: "ws://localhost:9222",
	}
	cfg.MetricsBuilderConfig = metadata.DefaultMetricsBuilderConfig()

	_, err := factory.CreateMetrics(
		context.Background(),
		receivertest.NewNopSettings(metadata.Type),
		cfg,
		consumertest.NewNop(),
	)
	assert.NoError(t, err)
}

func TestCreateMetricsReceiverInvalidConfig(t *testing.T) {
	factory := NewFactory()

	_, err := factory.CreateMetrics(
		context.Background(),
		receivertest.NewNopSettings(metadata.Type),
		&struct{}{},
		consumertest.NewNop(),
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config was not a Chromium receiver config")
}

func TestFactoryType(t *testing.T) {
	factory := NewFactory()
	assert.Equal(t, metadata.Type, factory.Type())
}
