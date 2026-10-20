package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// buildServerGGUF writes a minimal GGUF v3 header for listing tests.
func buildServerGGUF(t *testing.T, dir string, kvs [][2]any) string {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("GGUF")
	binary.Write(&buf, binary.LittleEndian, uint32(3))
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))

	for _, kv := range kvs {
		key := kv[0].(string)
		binary.Write(&buf, binary.LittleEndian, uint64(len(key)))
		buf.WriteString(key)
		switch v := kv[1].(type) {
		case string:
			binary.Write(&buf, binary.LittleEndian, uint32(8))
			binary.Write(&buf, binary.LittleEndian, uint64(len(v)))
			buf.WriteString(v)
		case uint32:
			binary.Write(&buf, binary.LittleEndian, uint32(4))
			binary.Write(&buf, binary.LittleEndian, v)
		default:
			t.Fatalf("unsupported GGUF fixture value %T", kv[1])
		}
	}

	path := filepath.Join(dir, "Qwen3.8-Flash-Next-UD-Q4_K_XL.gguf")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// llamaSwapMeta returns the meta.llamaswap block of a listing record.
func llamaSwapMeta(t *testing.T, rec modelRecord) map[string]any {
	t.Helper()
	require.NotNil(t, rec.Meta, "meta is missing")
	raw, ok := rec.Meta["llamaswap"]
	require.True(t, ok, "meta.llamaswap is missing")
	meta, ok := raw.(map[string]any)
	require.True(t, ok, "meta.llamaswap is not an object")
	return meta
}

func TestAPI_ListModelsContextPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		mc          config.ModelConfig
		discovered  int
		wantContext int
		wantSource  string
	}{
		{
			name:        "config beats discovered and cmd",
			mc:          config.ModelConfig{Capabilities: config.ModelCapConfig{Context: 999}, Inferred: config.ModelCapConfig{Context: 4096}},
			discovered:  8192,
			wantContext: 999,
			wantSource:  "config",
		},
		{
			name:        "discovered beats cmd",
			mc:          config.ModelConfig{Inferred: config.ModelCapConfig{Context: 4096}},
			discovered:  8192,
			wantContext: 8192,
			wantSource:  "discovered",
		},
		{
			name:        "cmd is the fallback",
			mc:          config.ModelConfig{Inferred: config.ModelCapConfig{Context: 4096}},
			wantContext: 4096,
			wantSource:  "cmd",
		},
		{
			name:        "disableAuto skips discovery and inference",
			mc:          config.ModelConfig{Capabilities: config.ModelCapConfig{DisableAuto: true}, Inferred: config.ModelCapConfig{Context: 4096}},
			discovered:  8192,
			wantContext: 0,
			wantSource:  "",
		},
		{
			name:        "disableAuto still reports configured context",
			mc:          config.ModelConfig{Capabilities: config.ModelCapConfig{DisableAuto: true, Context: 2048}, Inferred: config.ModelCapConfig{Context: 4096}},
			discovered:  8192,
			wantContext: 2048,
			wantSource:  "config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := tt.mc
			mc.Cmd = "llama-server -m a.gguf"
			mc.Proxy = "http://localhost:9001"
			s := capServer(t, "m", mc)
			if tt.discovered > 0 {
				seedCapabilities(t, s, "m", mc, config.ModelCapConfig{Context: tt.discovered})
			}

			rec := listModel(t, s)
			assert.Equal(t, tt.wantContext, rec.ContextLength)

			meta := llamaSwapMeta(t, rec)
			if tt.wantSource == "" {
				assert.NotContains(t, meta, "contextSource")
			} else {
				assert.Equal(t, tt.wantSource, meta["contextSource"])
			}
		})
	}
}

func TestAPI_ListModelsRendersScannedMetadata(t *testing.T) {
	dir := t.TempDir()
	modelPath := buildServerGGUF(t, dir, [][2]any{
		{"general.architecture", "qwen4exp"},
		{"general.name", "Qwen3.8 Flash Next"},
		{"general.description", "A Preview of the Qwen4 Architecture"},
		{"general.size_label", "512x56B"},
		{"qwen4exp.context_length", uint32(262144)},
		{"tokenizer.chat_template", "{%- if tools %}<tool_call>{%- endif %}"},
	})

	yaml := `
models:
  m:
    cmd: llama-server -m ` + modelPath + ` --ctx-size 32768 --parallel 2 --mmproj mmproj.gguf --model-draft /models/Qwen3-0.6B-Q8_0.gguf --spec-type draft-mtp --spec-draft-n-max 2 --jinja
    proxy: http://localhost:9001
    aliases: [m-alias]
`
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	s := capServer(t, "m", cfg.Models["m"])
	s.cfg.IncludeAliasesInList = true

	records := listModels(t, s)
	require.Len(t, records, 2, "model and alias")

	for _, rec := range records {
		assert.Equal(t, 16384, rec.ContextLength, "%s: runtime context is total/parallel", rec.ID)
		assert.Equal(t, 16384, rec.ContextWindow)
		assert.Equal(t, "Qwen3.8 Flash Next", rec.Name, "%s: name falls back to GGUF", rec.ID)
		assert.Equal(t, "A Preview of the Qwen4 Architecture", rec.Description)
		assert.Equal(t, []any{"text", "image"}, rec.Architecture["input_modalities"])
		assert.Equal(t, true, rec.Capabilities["vision"])
		assert.Equal(t, true, rec.Capabilities["function_calling"])
		assert.Contains(t, rec.SupportedParameters, "tools")
		assert.Contains(t, rec.SupportedParameters, "tool_choice")

		meta := llamaSwapMeta(t, rec)
		assert.Equal(t, "cmd", meta["contextSource"])
		assert.Equal(t, "qwen4exp", meta["modelArchitecture"])
		assert.Equal(t, "Q4_K_XL", meta["quantization"])
		assert.Equal(t, float64(262144), meta["trainedContext"])
		assert.Equal(t, "512x56B", meta["sizeLabel"])
		assert.Equal(t, "Qwen3-0.6B-Q8_0.gguf", meta["draft"])
		assert.Equal(t, "draft-mtp n-max=2", meta["spec"])
		assert.Equal(t, "gguf", meta["scanSource"])

		// The listing must not leak the command line or absolute paths.
		encoded, err := json.Marshal(rec)
		require.NoError(t, err)
		body := string(encoded)
		assert.NotContains(t, body, dir, "absolute model directory leaked")
		assert.NotContains(t, body, "--ctx-size", "command line leaked")
		assert.NotContains(t, body, "llama-server", "command line leaked")
	}
}

func TestAPI_ListModelsDiscoveredToolsBeatInference(t *testing.T) {
	mc := config.ModelConfig{
		Cmd:      "llama-server -m a.gguf --jinja",
		Proxy:    "http://localhost:9001",
		Inferred: config.ModelCapConfig{Tools: true},
	}

	// Before a probe, the heuristic supplies the tool capability.
	rec := listModel(t, capServer(t, "m", mc))
	assert.Equal(t, true, rec.Capabilities["function_calling"])

	// Once the model has been probed, a discovered false is authoritative.
	s := capServer(t, "m", mc)
	seedCapabilities(t, s, "m", mc, config.ModelCapConfig{Tools: false})
	rec = listModel(t, s)
	assert.NotContains(t, rec.Capabilities, "function_calling")
	assert.NotContains(t, rec.SupportedParameters, "tools")
}

func TestAPI_ListModelsUserMetadataOverridesScanned(t *testing.T) {
	mc := config.ModelConfig{
		Cmd:   "llama-server -m a.gguf --ctx-size 4096",
		Proxy: "http://localhost:9001",
		Name:  "Hand written name",
		Scanned: config.ModelScanInfo{
			Name:         "GGUF name",
			Architecture: "qwen4exp",
			Quantization: "Q4_K_M",
			SizeLabel:    "8B",
			Source:       "gguf",
		},
		Metadata: map[string]any{
			"quantization":      "custom-quant",
			"modelArchitecture": "custom-arch",
			"scanSource":        "manual",
		},
	}

	rec := listModel(t, capServer(t, "m", mc))
	assert.Equal(t, "Hand written name", rec.Name)

	meta := llamaSwapMeta(t, rec)
	assert.Equal(t, "custom-quant", meta["quantization"], "user metadata wins")
	assert.Equal(t, "custom-arch", meta["modelArchitecture"], "user metadata wins")
	assert.Equal(t, "manual", meta["scanSource"], "user metadata wins")
	assert.Equal(t, "8B", meta["sizeLabel"], "unset keys still come from the scan")
}
