// Package modelmeta reads the metadata header of a GGUF model file.
//
// Only the key/value metadata at the start of the file is read. Tensor data,
// which is almost all of a model file, is never touched, so inspecting a
// multi-gigabyte model costs a few milliseconds and a small read buffer.
//
// The parser is deliberately forgiving about values it does not use, but
// strict about the shape of the file: a truncated or malformed header returns
// an error rather than a partial result.
package modelmeta

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
)

// Limits that keep a hostile or corrupt header from making the parser
// allocate or read without bound. Real model headers are far below these.
const (
	maxMetadataBytes = 32 << 20
	maxStringBytes   = 8 << 20
	maxKVPairs       = 1 << 20
	maxArrayElements = 1 << 24
	maxArrayDepth    = 4
)

// ErrNotGGUF is returned when the file does not start with the GGUF magic.
var ErrNotGGUF = errors.New("modelmeta: not a GGUF file")

// Metadata is the subset of GGUF metadata llama-swap uses.
type Metadata struct {
	// Name is general.name.
	Name string
	// Description is general.description.
	Description string
	// Architecture is general.architecture, e.g. "llama" or "qwen4exp".
	Architecture string
	// SizeLabel is general.size_label, a human label such as "512x56B".
	SizeLabel string
	// TrainedContext is <architecture>.context_length: the window the model
	// was trained for, not the context a server was started with.
	TrainedContext int
	// FileType is general.file_type, the raw GGUF enum. It is reported as-is
	// because it is a poor guide to dynamic quantization names.
	FileType uint32
	// ChatTemplate is tokenizer.chat_template, when the file carries one.
	ChatTemplate string
}

// GGUF value types.
const (
	typeUint8 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// toolsGuardPattern matches the template guard a tool-capable chat template
// uses to render a tool list, e.g. {%- if tools %} or
// {%- if tools and tools is iterable %}.
var toolsGuardPattern = regexp.MustCompile(`(?i)\{%-?\s*if\s+tools\b`)

// SupportsTools reports whether a chat template appears to support tools. It
// is a conservative pre-load heuristic: the template must both guard on the
// tools variable and name a tool-call representation. The loaded server's
// capability probe remains authoritative once it runs.
func SupportsTools(template string) bool {
	if template == "" || !toolsGuardPattern.MatchString(template) {
		return false
	}
	lower := strings.ToLower(template)
	for _, marker := range []string{
		"tool_call",
		"tool_calls",
		"tool_choice",
		"function_call",
		"<function=",
		"<tool_call>",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ReadFile reads the metadata header of the GGUF file at path.
func ReadFile(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	return Read(f)
}

// Read reads a GGUF metadata header from r. It stops after the last key/value
// pair and never reads tensor data.
func Read(r io.Reader) (Metadata, error) {
	counted := &countingReader{r: r, max: maxMetadataBytes}

	var magic [4]byte
	if _, err := io.ReadFull(counted, magic[:]); err != nil {
		return Metadata{}, truncated(err)
	}
	if string(magic[:]) != "GGUF" {
		return Metadata{}, ErrNotGGUF
	}

	var version uint32
	if err := binary.Read(counted, binary.LittleEndian, &version); err != nil {
		return Metadata{}, truncated(err)
	}
	if version < 2 || version > 3 {
		return Metadata{}, fmt.Errorf("modelmeta: unsupported GGUF version %d", version)
	}

	var tensorCount, kvCount uint64
	if err := binary.Read(counted, binary.LittleEndian, &tensorCount); err != nil {
		return Metadata{}, truncated(err)
	}
	if err := binary.Read(counted, binary.LittleEndian, &kvCount); err != nil {
		return Metadata{}, truncated(err)
	}
	if kvCount > maxKVPairs {
		return Metadata{}, fmt.Errorf("modelmeta: implausible metadata key count %d", kvCount)
	}

	var meta Metadata
	contexts := make(map[string]uint64)

	for i := uint64(0); i < kvCount; i++ {
		key, err := readString(counted)
		if err != nil {
			return Metadata{}, err
		}
		var valueType uint32
		if err := binary.Read(counted, binary.LittleEndian, &valueType); err != nil {
			return Metadata{}, truncated(err)
		}

		if strings.HasSuffix(key, ".context_length") {
			arch := strings.TrimSuffix(key, ".context_length")
			value, err := readValue(counted, valueType, 0)
			if err != nil {
				return Metadata{}, err
			}
			if n, ok := value.asUint64(); ok {
				contexts[arch] = n
			}
			continue
		}

		switch key {
		case "general.name", "general.description", "general.architecture", "general.size_label":
			value, err := readValue(counted, valueType, 0)
			if err != nil {
				return Metadata{}, err
			}
			text, _ := value.asString()
			switch key {
			case "general.name":
				meta.Name = text
			case "general.description":
				meta.Description = text
			case "general.architecture":
				meta.Architecture = text
			case "general.size_label":
				meta.SizeLabel = text
			}
		case "general.file_type":
			value, err := readValue(counted, valueType, 0)
			if err != nil {
				return Metadata{}, err
			}
			if n, ok := value.asUint64(); ok && n <= math.MaxUint32 {
				meta.FileType = uint32(n)
			}
		case "tokenizer.chat_template":
			value, err := readValue(counted, valueType, 0)
			if err != nil {
				return Metadata{}, err
			}
			if text, ok := value.asString(); ok {
				meta.ChatTemplate = text
			}
		default:
			if err := skipValue(counted, valueType, 0); err != nil {
				return Metadata{}, err
			}
		}
	}

	if n, ok := contexts[meta.Architecture]; ok && meta.Architecture != "" {
		if n <= uint64(maxInt()) {
			meta.TrainedContext = int(n)
		}
	}

	return meta, nil
}

// countingReader fails once more than max bytes have passed through it.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, fmt.Errorf("modelmeta: metadata exceeds %d bytes", c.max)
	}
	return n, err
}

// value holds a scalar decoded from the header. Arrays are skipped, so only
// scalars ever reach it.
type value struct {
	u uint64
	i int64
	f float64
	s string
	b bool
}

func (v value) asString() (string, bool) {
	return v.s, v.s != ""
}

func (v value) asUint64() (uint64, bool) {
	switch {
	case v.u != 0:
		return v.u, true
	case v.i > 0:
		return uint64(v.i), true
	default:
		// Zero values cannot be distinguished from an unset field, which is
		// fine for the context and file_type values this package reports.
		return 0, false
	}
}

func readString(r io.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", truncated(err)
	}
	if n > maxStringBytes {
		return "", fmt.Errorf("modelmeta: oversized string (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", truncated(err)
	}
	return string(buf), nil
}

// skipString consumes a length-prefixed string without keeping it.
func skipString(r io.Reader) error {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return truncated(err)
	}
	if n > maxStringBytes {
		return fmt.Errorf("modelmeta: oversized string (%d bytes)", n)
	}
	if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
		return truncated(err)
	}
	return nil
}

func readValue(r io.Reader, valueType uint32, depth int) (value, error) {
	switch valueType {
	case typeUint8:
		var v uint8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{u: uint64(v)}, nil
	case typeInt8:
		var v int8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{i: int64(v)}, nil
	case typeUint16:
		var v uint16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{u: uint64(v)}, nil
	case typeInt16:
		var v int16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{i: int64(v)}, nil
	case typeUint32:
		var v uint32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{u: uint64(v)}, nil
	case typeInt32:
		var v int32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{i: int64(v)}, nil
	case typeFloat32:
		var v float32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{f: float64(v)}, nil
	case typeBool:
		var v uint8
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{b: v != 0}, nil
	case typeString:
		s, err := readString(r)
		return value{s: s}, err
	case typeArray:
		if depth >= maxArrayDepth {
			return value{}, errors.New("modelmeta: arrays nested too deeply")
		}
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return value{}, truncated(err)
		}
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return value{}, truncated(err)
		}
		if count > maxArrayElements {
			return value{}, fmt.Errorf("modelmeta: implausible array length %d", count)
		}
		for i := uint64(0); i < count; i++ {
			if err := skipValue(r, elemType, depth+1); err != nil {
				return value{}, err
			}
		}
		return value{}, nil
	case typeUint64:
		var v uint64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{u: v}, nil
	case typeInt64:
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{i: v}, nil
	case typeFloat64:
		var v float64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return value{}, truncated(err)
		}
		return value{f: v}, nil
	default:
		return value{}, fmt.Errorf("modelmeta: unknown GGUF value type %d", valueType)
	}
}

func skipValue(r io.Reader, valueType uint32, depth int) error {
	switch valueType {
	case typeUint8, typeInt8, typeBool:
		_, err := io.CopyN(io.Discard, r, 1)
		return truncated(err)
	case typeUint16, typeInt16:
		_, err := io.CopyN(io.Discard, r, 2)
		return truncated(err)
	case typeUint32, typeInt32, typeFloat32:
		_, err := io.CopyN(io.Discard, r, 4)
		return truncated(err)
	case typeUint64, typeInt64, typeFloat64:
		_, err := io.CopyN(io.Discard, r, 8)
		return truncated(err)
	case typeString:
		return skipString(r)
	default:
		_, err := readValue(r, valueType, depth)
		return err
	}
}

func truncated(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("modelmeta: truncated GGUF metadata: %w", err)
	}
	return err
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
