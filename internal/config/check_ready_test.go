package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConfig_CheckReadyValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "jsonPath",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      jsonPath: "$.model"
`,
		},
		{
			name: "bodyRegex",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      bodyRegex: '"loading_model"\s*:\s*null'
`,
		},
		{
			name: "checkEndpoint none",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkEndpoint: none
    checkReady:
      jsonPath: "$.model"
`,
			wantErr: "requires a checkEndpoint other than 'none'",
		},
		{
			name: "neither predicate",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady: {}
`,
			wantErr: "exactly one of jsonPath or bodyRegex",
		},
		{
			name: "both predicates",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      jsonPath: "$.model"
      bodyRegex: ready
`,
			wantErr: "exactly one of jsonPath or bodyRegex",
		},
		{
			name: "equals without jsonPath",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      bodyRegex: ready
      equals: yes
`,
			wantErr: "require jsonPath",
		},
		{
			name: "equals and notEquals",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      jsonPath: "$.model"
      equals: one
      notEquals: two
`,
			wantErr: "cannot set both equals and notEquals",
		},
		{
			name: "invalid bodyRegex",
			content: `
models:
  m:
    cmd: echo m --port ${PORT}
    checkReady:
      bodyRegex: "("
`,
			wantErr: "invalid regular expression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(tt.content))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func mustCheckReady(t *testing.T, content string) *CheckReadyConfig {
	t.Helper()
	var cr CheckReadyConfig
	require.NoError(t, yaml.Unmarshal([]byte(content), &cr))
	return &cr
}

func TestCheckReadyConfig_Matches(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		body    string
		matches bool
	}{
		{"jsonPath present", `jsonPath: "$.model"`, `{"model":"qwen3.8:flash-next"}`, true},
		{"jsonPath null", `jsonPath: "$.model"`, `{"model":null}`, false},
		{"jsonPath false", `jsonPath: "$.model"`, `{"model":false}`, false},
		{"jsonPath missing", `jsonPath: "$.model"`, `{"status":"loading"}`, false},
		{"equals string", `jsonPath: "$.model"
equals: qwen3.8:flash-next`, `{"model":"qwen3.8:flash-next"}`, true},
		{"equals mismatch", `jsonPath: "$.model"
equals: expected`, `{"model":"other"}`, false},
		{"notEquals null", `jsonPath: "$.model"
notEquals: null`, `{"model":"loaded"}`, true},
		{"notEquals null when null", `jsonPath: "$.model"
notEquals: null`, `{"model":null}`, false},
		{"bodyRegex", `bodyRegex: '"loading_model"\s*:\s*null'`, `{"loading_model":null}`, true},
		{"bodyRegex no match", `bodyRegex: '"loading_model"\s*:\s*null'`, `{"model":"qwen"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := mustCheckReady(t, tt.config)
			assert.Equal(t, tt.matches, cr.Matches([]byte(tt.body)))
		})
	}

	t.Run("nil config matches", func(t *testing.T) {
		var cr *CheckReadyConfig
		assert.True(t, cr.Matches([]byte("anything")))
	})

	t.Run("oversized body does not match", func(t *testing.T) {
		cr := &CheckReadyConfig{JSONPath: "$.model"}
		body := `{"model":"ok","padding":"` + strings.Repeat("x", maxCheckReadyBodyLen) + `"}`
		assert.False(t, cr.Matches([]byte(body)))
	})
}
