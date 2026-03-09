// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package chromiumreceiver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		expectedErr string
	}{
		{
			name: "valid config",
			config: Config{
				Endpoint: "ws://localhost:9222",
			},
			expectedErr: "",
		},
		{
			name: "valid config with wss",
			config: Config{
				Endpoint: "wss://localhost:9222",
			},
			expectedErr: "",
		},
		{
			name:        "missing endpoint",
			config:      Config{},
			expectedErr: `"endpoint" must be specified`,
		},
		{
			name: "invalid endpoint scheme",
			config: Config{
				Endpoint: "http://localhost:9222",
			},
			expectedErr: `"endpoint" must use ws:// or wss:// scheme`,
		},
		{
			name: "invalid endpoint format",
			config: Config{
				Endpoint: "not-a-url",
			},
			expectedErr: `"endpoint" must use ws:// or wss:// scheme`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectedErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
			}
		})
	}
}
