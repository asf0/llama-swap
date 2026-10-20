package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadConfigForTest(t *testing.T, content string) Config {
	t.Helper()
	cfg, err := LoadConfigFromReader(strings.NewReader(content))
	require.NoError(t, err)
	return cfg
}

func TestConfig_HealthCheckTimeoutPerModel(t *testing.T) {
	cfg := loadConfigForTest(t, `
healthCheckTimeout: 200
models:
  inherit:
    cmd: echo inherit --port ${PORT}
  override:
    cmd: echo override --port ${PORT}
    healthCheckTimeout: 900
  below-minimum:
    cmd: echo low --port ${PORT}
    healthCheckTimeout: 5
`)

	assert.Equal(t, 200, cfg.HealthCheckTimeout)
	assert.Equal(t, 200, cfg.Models["inherit"].HealthCheckTimeout)
	assert.Equal(t, 900, cfg.Models["override"].HealthCheckTimeout)
	assert.Equal(t, 15, cfg.Models["below-minimum"].HealthCheckTimeout)
}
