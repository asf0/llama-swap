package modelmeta

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// testKV is one metadata pair for the synthetic fixture builder.
type testKV struct {
	key   string
	value any
}

func writeString(buf *bytes.Buffer, s string) {
	binary.Write(buf, binary.LittleEndian, uint64(len(s)))
	buf.WriteString(s)
}

func writeValue(t *testing.T, buf *bytes.Buffer, value any) {
	t.Helper()
	switch v := value.(type) {
	case uint8:
		binary.Write(buf, binary.LittleEndian, uint32(typeUint8))
		binary.Write(buf, binary.LittleEndian, v)
	case uint16:
		binary.Write(buf, binary.LittleEndian, uint32(typeUint16))
		binary.Write(buf, binary.LittleEndian, v)
	case uint32:
		binary.Write(buf, binary.LittleEndian, uint32(typeUint32))
		binary.Write(buf, binary.LittleEndian, v)
	case int32:
		binary.Write(buf, binary.LittleEndian, uint32(typeInt32))
		binary.Write(buf, binary.LittleEndian, v)
	case uint64:
		binary.Write(buf, binary.LittleEndian, uint32(typeUint64))
		binary.Write(buf, binary.LittleEndian, v)
	case bool:
		binary.Write(buf, binary.LittleEndian, uint32(typeBool))
		if v {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	case float32:
		binary.Write(buf, binary.LittleEndian, uint32(typeFloat32))
		binary.Write(buf, binary.LittleEndian, v)
	case string:
		binary.Write(buf, binary.LittleEndian, uint32(typeString))
		writeString(buf, v)
	case []string:
		binary.Write(buf, binary.LittleEndian, uint32(typeArray))
		binary.Write(buf, binary.LittleEndian, uint32(typeString))
		binary.Write(buf, binary.LittleEndian, uint64(len(v)))
		for _, item := range v {
			writeString(buf, item)
		}
	default:
		t.Fatalf("unsupported fixture value %T", value)
	}
}

// buildGGUF returns a synthetic GGUF header with no tensors.
func buildGGUF(t *testing.T, version uint32, kvs []testKV) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	binary.Write(&buf, binary.LittleEndian, version)
	binary.Write(&buf, binary.LittleEndian, uint64(0)) // tensor count
	binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	for _, kv := range kvs {
		writeString(&buf, kv.key)
		writeValue(t, &buf, kv.value)
	}
	return buf.Bytes()
}

func TestGGUF_GGUFMetadata(t *testing.T) {
	data := buildGGUF(t, 3, []testKV{
		{"general.architecture", "qwen4exp"},
		{"general.name", "Qwen3.8 Flash Next"},
		{"general.description", "A Preview of the Qwen4 Architecture"},
		{"general.size_label", "512x56B"},
		{"general.file_type", uint32(15)},
		{"general.tags", []string{"one", "two", "three"}},
		{"qwen4exp.context_length", uint32(262144)},
		{"tokenizer.chat_template", "{%- if tools %}<tool_call>{%- endif %}"},
	})

	meta, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if meta.Architecture != "qwen4exp" {
		t.Errorf("Architecture = %q", meta.Architecture)
	}
	if meta.Name != "Qwen3.8 Flash Next" {
		t.Errorf("Name = %q", meta.Name)
	}
	if meta.Description != "A Preview of the Qwen4 Architecture" {
		t.Errorf("Description = %q", meta.Description)
	}
	if meta.SizeLabel != "512x56B" {
		t.Errorf("SizeLabel = %q", meta.SizeLabel)
	}
	if meta.TrainedContext != 262144 {
		t.Errorf("TrainedContext = %d", meta.TrainedContext)
	}
	if meta.FileType != 15 {
		t.Errorf("FileType = %d", meta.FileType)
	}
	if meta.ChatTemplate != "{%- if tools %}<tool_call>{%- endif %}" {
		t.Errorf("ChatTemplate = %q", meta.ChatTemplate)
	}
}

func TestSupportsTools(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     bool
	}{
		{
			name:     "qwen tool template",
			template: `{%- if tools and tools is iterable %}...<tool_call><function=name>...</function></tool_call>{%- endif %}`,
			want:     true,
		},
		{
			name:     "gemma tool template",
			template: `{%- if tools -%}{{ tool_calls }}{%- endif %}`,
			want:     true,
		},
		{
			name:     "no tools guard",
			template: `{%- for message in messages %}{{ message.content }}{%- endfor %}`,
			want:     false,
		},
		{
			name:     "guard without call format",
			template: `{%- if tools %}tools are not supported here{%- endif %}`,
			want:     false,
		},
		{
			name:     "empty",
			template: "",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsTools(tt.template); got != tt.want {
				t.Errorf("SupportsTools() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGGUF_SkipsArraysAndUnknownValues(t *testing.T) {
	data := buildGGUF(t, 3, []testKV{
		{"general.tags", []string{"a", "b"}},
		{"some.unknown", uint16(7)},
		{"general.architecture", "llama"},
		{"llama.context_length", uint64(131072)},
		{"other.context_length", uint32(4096)},
	})

	meta, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if meta.TrainedContext != 131072 {
		t.Errorf("TrainedContext = %d, want the architecture's own context", meta.TrainedContext)
	}
	if meta.Architecture != "llama" {
		t.Errorf("Architecture = %q", meta.Architecture)
	}
}

func TestGGUF_MalformedMagic(t *testing.T) {
	_, err := Read(bytes.NewReader([]byte("NOPE\x03\x00\x00\x00")))
	if err == nil {
		t.Fatal("Read() accepted a file without the GGUF magic")
	}
}

func TestGGUF_UnsupportedVersion(t *testing.T) {
	data := buildGGUF(t, 1, nil)
	if _, err := Read(bytes.NewReader(data)); err == nil {
		t.Fatal("Read() accepted version 1")
	}
}

func TestGGUF_TruncatedMetadata(t *testing.T) {
	data := buildGGUF(t, 3, []testKV{
		{"general.architecture", "llama"},
		{"llama.context_length", uint32(4096)},
	})

	// Cut the header in the middle of the second value.
	if _, err := Read(bytes.NewReader(data[:len(data)-3])); err == nil {
		t.Fatal("Read() accepted truncated metadata")
	}
}

func TestGGUF_OversizedString(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	binary.Write(&buf, binary.LittleEndian, uint32(3))
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	binary.Write(&buf, binary.LittleEndian, uint64(1))
	writeString(&buf, "general.name")
	binary.Write(&buf, binary.LittleEndian, uint32(typeString))
	binary.Write(&buf, binary.LittleEndian, uint64(maxStringBytes+1))

	_, err := Read(bytes.NewReader(buf.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "oversized string") {
		t.Fatalf("Read() error = %v, want oversized string error", err)
	}
}

func TestGGUF_ImplausibleKeyCount(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	binary.Write(&buf, binary.LittleEndian, uint32(3))
	binary.Write(&buf, binary.LittleEndian, uint64(0))
	binary.Write(&buf, binary.LittleEndian, uint64(maxKVPairs+1))

	if _, err := Read(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("Read() accepted an implausible key count")
	}
}
