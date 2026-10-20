package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FixedProxyPortReservation(t *testing.T) {
	t.Run("Reserved Start Port", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-fixed:
    cmd: svr --port 5800
    proxy: http://127.0.0.1:5800
  b-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5800", config.Models["a-fixed"].Cmd)
		assert.Equal(t, "http://127.0.0.1:5800", config.Models["a-fixed"].Proxy)
		assert.Equal(t, "svr --port 5801", config.Models["b-auto"].Cmd)
		assert.Equal(t, "http://localhost:5801", config.Models["b-auto"].Proxy)
	})

	t.Run("Reserved Port Later in Sorted Order", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-auto:
    cmd: svr --port ${PORT}
  z-fixed:
    cmd: svr --port 5800
    proxy: http://localhost:5800
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5801", config.Models["a-auto"].Cmd)
		assert.Equal(t, "http://localhost:5801", config.Models["a-auto"].Proxy)
		assert.Equal(t, "svr --port 5800", config.Models["z-fixed"].Cmd)
		assert.Equal(t, "http://localhost:5800", config.Models["z-fixed"].Proxy)
	})

	t.Run("Remote Proxy Does Not Reserve", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-fixed:
    cmd: svr --port 5800
    proxy: http://192.168.1.10:5800
  b-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "http://192.168.1.10:5800", config.Models["a-fixed"].Proxy)
		assert.Equal(t, "svr --port 5800", config.Models["b-auto"].Cmd)
		assert.Equal(t, "http://localhost:5800", config.Models["b-auto"].Proxy)
	})

	t.Run("Reserved Port Below Start Port", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-fixed:
    cmd: svr --port 5799
    proxy: http://localhost:5799
  b-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5799", config.Models["a-fixed"].Cmd)
		assert.Equal(t, "svr --port 5800", config.Models["b-auto"].Cmd)
		assert.Equal(t, "http://localhost:5800", config.Models["b-auto"].Proxy)
	})

	t.Run("Two Automatic Models Around One Reserved Port", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-auto:
    cmd: svr --port ${PORT}
  b-fixed:
    cmd: svr --port 5801
    proxy: http://localhost:5801
  c-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5800", config.Models["a-auto"].Cmd)
		assert.Equal(t, "http://localhost:5800", config.Models["a-auto"].Proxy)
		assert.Equal(t, "svr --port 5801", config.Models["b-fixed"].Cmd)
		assert.Equal(t, "http://localhost:5801", config.Models["b-fixed"].Proxy)
		assert.Equal(t, "svr --port 5802", config.Models["c-auto"].Cmd)
		assert.Equal(t, "http://localhost:5802", config.Models["c-auto"].Proxy)
	})

	t.Run("Wildcard and IPv6 Loopback Reserve", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-any:
    cmd: svr --port 5800
    proxy: http://0.0.0.0:5800
  b-v6:
    cmd: svr --port 5801
    proxy: http://[::1]:5801
  c-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5802", config.Models["c-auto"].Cmd)
		assert.Equal(t, "http://localhost:5802", config.Models["c-auto"].Proxy)
	})

	t.Run("Proxy Without Explicit Port Does Not Reserve", func(t *testing.T) {
		content := `
startPort: 5800
models:
  a-fixed:
    cmd: svr --port 5800
    proxy: http://localhost
  b-auto:
    cmd: svr --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err)

		assert.Equal(t, "svr --port 5800", config.Models["b-auto"].Cmd)
		assert.Equal(t, "http://localhost:5800", config.Models["b-auto"].Proxy)
	})

	t.Run("Automatic Proxy Without Automatic Cmd Still Errors", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: svr --port 111
`
		_, err := LoadConfigFromReader(strings.NewReader(content))
		require.Error(t, err)
		assert.Equal(t, "model model1: proxy uses ${PORT} but cmd does not - ${PORT} is only available when used in cmd", err.Error())
	})
}
