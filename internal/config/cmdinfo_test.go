package config

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ggufTestKV struct {
	key   string
	value any
}

// buildTestGGUF writes a minimal GGUF v3 header to a temp file and returns its
// path. Only metadata pairs are written; there are no tensors.
func buildTestGGUF(t *testing.T, dir string, kvs []ggufTestKV) string {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("GGUF")
	binary.Write(&buf, binary.LittleEndian, uint32(3))
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))

	for _, kv := range kvs {
		binary.Write(&buf, binary.LittleEndian, uint64(len(kv.key)))
		buf.WriteString(kv.key)
		switch v := kv.value.(type) {
		case string:
			binary.Write(&buf, binary.LittleEndian, uint32(8))
			binary.Write(&buf, binary.LittleEndian, uint64(len(v)))
			buf.WriteString(v)
		case uint32:
			binary.Write(&buf, binary.LittleEndian, uint32(4))
			binary.Write(&buf, binary.LittleEndian, v)
		default:
			t.Fatalf("unsupported GGUF fixture value %T", kv.value)
		}
	}

	path := filepath.Join(dir, "model-UD-Q4_K_XL.gguf")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing GGUF fixture: %v", err)
	}
	return path
}

func TestInferCommandContext(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want int
	}{
		{"ctx-size", "llama-server --ctx-size 8192", 8192},
		{"ctx-size equals", "llama-server --ctx-size=4096", 4096},
		{"ctx-size with parallel", "llama-server --ctx-size 32768 --parallel 4", 8192},
		{"unified keeps full context", "llama-server --ctx-size 131072 --parallel 2 --kv-unified", 131072},
		{"unified short flag", "llama-server --ctx-size 262144 --parallel 2 -kvu", 262144},
		{"explicit non-unified", "llama-server --ctx-size 131072 --parallel 2 --kv-unified --no-kv-unified", 65536},
		{"explicit non-unified short", "llama-server --ctx-size 131072 --parallel 2 -kvu -no-kvu", 65536},
		{"unified per-slot cap", "llama-server --ctx-size 131072 --parallel 2 --kv-unified --kv-unified-per-slot 65536", 65536},
		{"unified per-slot above ctx", "llama-server --ctx-size 131072 --parallel 2 --kv-unified --kv-unified-per-slot 262144", 131072},
		{"short c", "llama-server -c 2048", 2048},
		{"short c equals", "llama-server -c=1024", 1024},
		{"context-size", "llama-server --context-size 3072", 3072},
		{"k suffix", "llama-server -c 128k", 131072},
		{"upper k suffix", "llama-server --ctx-size 64K", 65536},
		{"m suffix", "llama-server --ctx-size 1m", 1048576},
		{"max-seq", "hipfire --max-seq 4096", 4096},
		{"max-model-len", "vllm serve m --max-model-len 65536", 65536},
		{"context-length", "sglang --context-length 32768", 32768},
		{"missing", "llama-server -m model.gguf", 0},
		{"zero", "llama-server --ctx-size 0", 0},
		{"negative", "llama-server --ctx-size -1", 0},
		{"unparsable", "llama-server --ctx-size lots", 0},
		{"max-seq wins", "server --ctx-size 8192 --max-seq 4096", 4096},
		{"max-model-len over context-length", "server --context-length 8192 --max-model-len 16384", 16384},
		{"context-length over ctx-size", "server --ctx-size 8192 --context-length 16384", 16384},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := parseCommandInfo(strings.Fields(tt.cmd))
			if info.Context != tt.want {
				t.Errorf("Context = %d, want %d", info.Context, tt.want)
			}
		})
	}
}

func TestInferCommandVision(t *testing.T) {
	for _, cmd := range []string{
		"llama-server -m model.gguf --mmproj mmproj.gguf",
		"llama-server -m model.gguf --mm-proj=mmproj.gguf",
		"llama-server --mmproj",
	} {
		info := parseCommandInfo(strings.Fields(cmd))
		if !info.Vision {
			t.Errorf("%q: Vision = false, want true", cmd)
		}
	}

	info := parseCommandInfo(strings.Fields("llama-server -m model.gguf"))
	if info.Vision {
		t.Error("Vision = true without --mmproj")
	}
}

func TestScanModelConfig_MissingFile(t *testing.T) {
	mc := ModelConfig{Cmd: "llama-server -m /does/not/exist/Qwen3-8B-UD-Q4_K_XL.gguf --ctx-size 4096"}
	scanModelConfig(&mc)

	if mc.Inferred.Context != 4096 {
		t.Errorf("Inferred.Context = %d", mc.Inferred.Context)
	}
	if mc.Scanned.Quantization != "Q4_K_XL" {
		t.Errorf("Quantization = %q", mc.Scanned.Quantization)
	}
	if mc.Scanned.Name != "" || mc.Scanned.Architecture != "" {
		t.Errorf("missing file still produced GGUF fields: %+v", mc.Scanned)
	}
	if mc.Scanned.Source != "cmd" {
		t.Errorf("Source = %q, want cmd", mc.Scanned.Source)
	}
}

func TestScanModelConfig_NonGGUFModel(t *testing.T) {
	mc := ModelConfig{Cmd: "vllm serve /models/mistral --max-model-len 32768"}
	scanModelConfig(&mc)

	if mc.Scanned.Architecture != "" || mc.Scanned.Name != "" {
		t.Errorf("non-GGUF model produced file metadata: %+v", mc.Scanned)
	}
	if mc.Inferred.Context != 32768 {
		t.Errorf("Inferred.Context = %d", mc.Inferred.Context)
	}
}

func TestScanModelConfig_GGUFAndDraftSpec(t *testing.T) {
	dir := t.TempDir()
	modelPath := buildTestGGUF(t, dir, []ggufTestKV{
		{"general.architecture", "qwen4exp"},
		{"general.name", "Qwen3.8 Flash Next"},
		{"general.description", "A Preview of the Qwen4 Architecture"},
		{"general.size_label", "512x56B"},
		{"qwen4exp.context_length", uint32(262144)},
	})

	draftPath := filepath.Join(dir, "Qwen3-0.6B-Q8_0.gguf")
	mc := ModelConfig{Cmd: "llama-server -m " + modelPath +
		" --model-draft " + draftPath +
		" --spec-type draft-mtp --spec-draft-n-max 2" +
		" --ctx-size 32768 --parallel 2" +
		" --mmproj mmproj.gguf"}
	scanModelConfig(&mc)

	if mc.Inferred.Context != 16384 {
		t.Errorf("Inferred.Context = %d, want 32768/2", mc.Inferred.Context)
	}
	if len(mc.Inferred.In) != 2 || mc.Inferred.In[0] != "text" || mc.Inferred.In[1] != "image" {
		t.Errorf("Inferred.In = %v", mc.Inferred.In)
	}
	if mc.Scanned.Name != "Qwen3.8 Flash Next" {
		t.Errorf("Name = %q", mc.Scanned.Name)
	}
	if mc.Scanned.Description != "A Preview of the Qwen4 Architecture" {
		t.Errorf("Description = %q", mc.Scanned.Description)
	}
	if mc.Scanned.Architecture != "qwen4exp" {
		t.Errorf("Architecture = %q", mc.Scanned.Architecture)
	}
	if mc.Scanned.TrainedContext != 262144 {
		t.Errorf("TrainedContext = %d", mc.Scanned.TrainedContext)
	}
	if mc.Scanned.SizeLabel != "512x56B" {
		t.Errorf("SizeLabel = %q", mc.Scanned.SizeLabel)
	}
	if mc.Scanned.Quantization != "Q4_K_XL" {
		t.Errorf("Quantization = %q", mc.Scanned.Quantization)
	}
	if mc.Scanned.Draft != "Qwen3-0.6B-Q8_0.gguf" {
		t.Errorf("Draft = %q", mc.Scanned.Draft)
	}
	if mc.Scanned.Spec != "draft-mtp n-max=2" {
		t.Errorf("Spec = %q", mc.Scanned.Spec)
	}
	if mc.Scanned.Source != "gguf" {
		t.Errorf("Source = %q, want gguf", mc.Scanned.Source)
	}
}

func TestQuantizationFromFilename(t *testing.T) {
	tests := map[string]string{
		"Qwen3-8B-UD-Q4_K_XL.gguf":          "Q4_K_XL",
		"model-Q8_0.gguf":                   "Q8_0",
		"model-Q6_K.gguf":                   "Q6_K",
		"model-IQ4_XS.gguf":                 "IQ4_XS",
		"model-BF16.gguf":                   "BF16",
		"model-F16.gguf":                    "F16",
		"hipfire-model.mq4-xts":             "MQ4-XTS",
		"Qwen3.8-Flash-Next-UD-Q4_K_XL.zip": "Q4_K_XL",
		"plain-model.gguf":                  "",
	}

	for filename, want := range tests {
		if got := quantizationFromFilename("/models/" + filename); got != want {
			t.Errorf("quantizationFromFilename(%q) = %q, want %q", filename, got, want)
		}
	}
}

func TestScanModelConfig_CachesGGUFHeader(t *testing.T) {
	dir := t.TempDir()
	modelPath := buildTestGGUF(t, dir, []ggufTestKV{
		{"general.architecture", "llama"},
		{"llama.context_length", uint32(4096)},
	})

	first := ModelConfig{Cmd: "llama-server -m " + modelPath}
	scanModelConfig(&first)

	info, err := os.Stat(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	key := ggufCacheKey{path: modelPath, modTime: info.ModTime().UnixNano(), size: info.Size()}
	if _, ok := ggufCache.Load(key); !ok {
		t.Fatal("GGUF header was not cached")
	}

	second := ModelConfig{Cmd: "llama-server -m " + modelPath}
	scanModelConfig(&second)
	if second.Scanned.Architecture != first.Scanned.Architecture {
		t.Fatalf("cached scan differs: %+v vs %+v", first.Scanned, second.Scanned)
	}
}

func TestLoadConfig_PopulatesScanAndInference(t *testing.T) {
	dir := t.TempDir()
	modelPath := buildTestGGUF(t, dir, []ggufTestKV{
		{"general.architecture", "qwen4exp"},
		{"general.name", "Qwen3.8 Flash Next"},
		{"qwen4exp.context_length", uint32(262144)},
		{"tokenizer.chat_template", "{%- if tools %}<tool_call>{%- endif %}"},
	})

	yaml := `
models:
  m:
    cmd: llama-server -m ` + modelPath + ` --ctx-size 32768 --parallel 2 --jinja
    proxy: http://localhost:9001
`
	config, err := LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromReader() error = %v", err)
	}

	mc := config.Models["m"]
	if mc.Inferred.Context != 16384 {
		t.Errorf("Inferred.Context = %d", mc.Inferred.Context)
	}
	if mc.Scanned.Name != "Qwen3.8 Flash Next" {
		t.Errorf("Scanned.Name = %q", mc.Scanned.Name)
	}
	if mc.Scanned.TrainedContext != 262144 {
		t.Errorf("Scanned.TrainedContext = %d", mc.Scanned.TrainedContext)
	}
	if mc.Capabilities.Context != 0 {
		t.Errorf("inference leaked into Capabilities: %+v", mc.Capabilities)
	}
	if !mc.Inferred.Tools {
		t.Error("Inferred.Tools = false, want true for a tool-capable chat template")
	}
}

func TestScanModelConfig_ToolsNeedTextAndJinja(t *testing.T) {
	dir := t.TempDir()
	modelPath := buildTestGGUF(t, dir, []ggufTestKV{
		{"general.architecture", "qwen4exp"},
		{"tokenizer.chat_template", "{%- if tools %}<tool_call>{%- endif %}"},
	})

	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"jinja model", "llama-server -m " + modelPath + " --jinja", true},
		{"no jinja", "llama-server -m " + modelPath + " --no-jinja", false},
		{"embedding", "llama-server -m " + modelPath + " --jinja --embedding", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := ModelConfig{Cmd: tt.cmd}
			scanModelConfig(&mc)
			if mc.Inferred.Tools != tt.want {
				t.Errorf("Inferred.Tools = %v, want %v", mc.Inferred.Tools, tt.want)
			}
		})
	}
}
