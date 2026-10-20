package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_UiURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		uiURL   string
		wantErr string
	}{
		{name: "empty", uiURL: ""},
		{name: "http absolute", uiURL: "http://host:19095/ui/"},
		{name: "https absolute", uiURL: "https://host/ui/"},
		{name: "root relative", uiURL: "/ui/"},
		{name: "root relative bare", uiURL: "/"},
		{name: "javascript", uiURL: "javascript:alert(1)", wantErr: "must be an absolute"},
		{name: "protocol relative", uiURL: "//evil.example/ui/", wantErr: "must not be protocol-relative"},
		{name: "ftp", uiURL: "ftp://host/ui/", wantErr: "must be an absolute"},
		{name: "absolute without host", uiURL: "http://", wantErr: "missing a host"},
		{name: "relative path", uiURL: "ui/", wantErr: "must be an absolute"},
		{name: "bare host", uiURL: "host:19095", wantErr: "must be an absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := `
models:
  m:
    cmd: echo ${PORT}
    uiUrl: "` + tt.uiURL + `"
`
			cfg, err := LoadConfigFromReader(strings.NewReader(content))
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.uiURL, cfg.Models["m"].UiURL)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "uiUrl")
		})
	}
}
