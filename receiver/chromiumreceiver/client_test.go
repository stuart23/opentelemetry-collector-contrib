// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestCDPClientConnectFailure(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewCDPClient("ws://localhost:19/invalid", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to CDP endpoint")
}

func TestCDPClientDisconnectWithoutConnect(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewCDPClient("ws://localhost:9222", logger)

	err := client.Disconnect()
	assert.NoError(t, err)
}

func TestNewCDPClient(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewCDPClient("ws://localhost:9222", logger)

	assert.NotNil(t, client)
	assert.Equal(t, "ws://localhost:9222", client.endpoint)
}
