package config

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mostlygeek/llama-swap/internal/modelmeta"
)

// ModelScanInfo holds model facts read from the command line or from the GGUF
// header before the model is ever loaded. It is best-effort: every field may
// be empty, and none of it is validated as configuration.
type ModelScanInfo struct {
	Name           string
	Description    string
	Architecture   string
	Quantization   string
	TrainedContext int
	SizeLabel      string
	Draft          string
	Spec           string
	// Source is "gguf" when a header was read, "cmd" when only the command
	// line supplied facts, and empty when nothing was found.
	Source string
}

// commandInfo is what scanning one command line produces.
type commandInfo struct {
	Context   int
	Vision    bool
	NoJinja   bool
	Embedding bool
	ModelPath string
	DraftPath string
	Spec      string
}

// quantizationPattern matches the quant tokens model filenames use. The
// Unsloth dynamic prefix is captured separately so it can be stripped: the
// listing wants Q4_K_XL, not UD-Q4_K_XL.
var quantizationPattern = regexp.MustCompile(`(?i)(?:^|[-_.])(ud-)?((?:iq|q)\d+(?:_[a-z0-9]+)+|bf16|fp16|f16|f32|fp8|mq\d+(?:-[a-z0-9]+)+)(?:[-_.]|$)`)

// scanModelConfig fills the pre-load inference fields of mc. It never returns
// an error: a missing model file or an unparsable command just leaves the
// corresponding fields empty.
func scanModelConfig(mc *ModelConfig) {
	mc.Inferred = ModelCapConfig{}
	mc.Scanned = ModelScanInfo{}

	args, err := mc.SanitizedCommand()
	if err != nil {
		return
	}

	info := parseCommandInfo(args)
	inferred := ModelCapConfig{Context: info.Context}
	if info.Vision {
		inferred.In = []string{"text", "image"}
	}

	scan := ModelScanInfo{
		Draft: basename(info.DraftPath),
		Spec:  info.Spec,
	}
	if info.ModelPath != "" {
		scan.Quantization = quantizationFromFilename(info.ModelPath)
	}

	if info.ModelPath != "" && isGGUFPath(info.ModelPath) {
		if meta, ok := cachedGGUFMetadata(info.ModelPath); ok {
			scan.Name = meta.Name
			scan.Description = meta.Description
			scan.Architecture = meta.Architecture
			scan.SizeLabel = meta.SizeLabel
			scan.TrainedContext = meta.TrainedContext
			scan.Source = "gguf"
			// Tools are only meaningful when the chat template is actually
			// used and the model can generate text. Embedding servers have a
			// template too, but they do not emit tool calls.
			if !info.Embedding && !info.NoJinja && modelmeta.SupportsTools(meta.ChatTemplate) {
				inferred.Tools = true
			}
		}
	}

	mc.Inferred = inferred
	if scan.Source == "" && scan.hasFacts() {
		scan.Source = "cmd"
	}
	mc.Scanned = scan
}

// hasFacts reports whether any scanned field carries a value.
func (s ModelScanInfo) hasFacts() bool {
	return s.Name != "" || s.Description != "" || s.Architecture != "" ||
		s.Quantization != "" || s.TrainedContext > 0 || s.SizeLabel != "" ||
		s.Draft != "" || s.Spec != ""
}

// parseCommandInfo reads the inference flags llama-swap knows how to
// interpret. Unknown flags are ignored.
func parseCommandInfo(args []string) commandInfo {
	var info commandInfo

	// Runtime context. Servers spell it differently; the first flag that
	// yields a usable number wins, matching which one is most specific to a
	// backend when a wrapper passes several through.
	if ctx := flagInt(args, "--max-seq"); ctx > 0 {
		info.Context = ctx
	} else if ctx := flagInt(args, "--max-model-len"); ctx > 0 {
		info.Context = ctx
	} else if ctx := flagInt(args, "--context-length"); ctx > 0 {
		info.Context = ctx
	} else {
		ctx := flagInt(args, "--ctx-size")
		if ctx <= 0 {
			ctx = flagInt(args, "--context-size")
		}
		if ctx <= 0 {
			ctx = flagInt(args, "-c")
		}
		if ctx > 0 {
			// With a unified KV cache llama.cpp sets n_ctx_seq = n_ctx, so
			// the per-slot window is the full --ctx-size. Only divide when
			// KV caches are per slot.
			unified := hasFlag(args, "--kv-unified") || hasFlag(args, "-kvu")
			if hasFlag(args, "--no-kv-unified") || hasFlag(args, "-no-kvu") {
				unified = false
			}
			if !unified {
				parallel := flagInt(args, "--parallel")
				if parallel <= 0 {
					parallel = 1
				}
				ctx = ctx / parallel
			}
			if cap := flagInt(args, "--kv-unified-per-slot"); cap > 0 && ctx > cap {
				ctx = cap
			}
			info.Context = ctx
		}
	}

	info.Vision = hasFlag(args, "--mmproj") || hasFlag(args, "--mm-proj")
	info.NoJinja = hasFlag(args, "--no-jinja")
	info.Embedding = hasFlag(args, "--embedding") || hasFlag(args, "--embeddings")

	if value, ok := flagValue(args, "--model", "-m"); ok {
		info.ModelPath = value
	}
	if value, ok := flagValue(args, "--model-draft"); ok {
		info.DraftPath = value
	}

	specType, _ := flagValue(args, "--spec-type")
	if spec := strings.TrimSpace(specType); spec != "" {
		info.Spec = spec
		if nMax, ok := flagValue(args, "--spec-draft-n-max"); ok {
			if nMax = strings.TrimSpace(nMax); nMax != "" {
				info.Spec += " n-max=" + nMax
			}
		}
	}

	return info
}

// flagValue returns the last value given for any of names. It accepts
// "--flag value" and "--flag=value" (and "-f value", "-f=value").
func flagValue(args []string, names ...string) (string, bool) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	var result string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := arg
		value := ""
		hasValue := false
		if eq := strings.IndexByte(arg, '='); eq >= 0 {
			name = arg[:eq]
			value = arg[eq+1:]
			hasValue = true
		}
		if _, ok := wanted[name]; !ok {
			continue
		}
		if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value = args[i+1]
			hasValue = true
			i++
		}
		if hasValue {
			result = value
			found = true
		}
	}
	return result, found
}

// hasFlag reports whether any of names appears, with or without a value.
func hasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		name := arg
		if eq := strings.IndexByte(arg, '='); eq >= 0 {
			name = arg[:eq]
		}
		for _, want := range names {
			if name == want {
				return true
			}
		}
	}
	return false
}

// flagInt returns the last numeric value of a flag, or 0 when the flag is
// missing, negative or unparsable.
func flagInt(args []string, names ...string) int {
	raw, ok := flagValue(args, names...)
	if !ok {
		return 0
	}
	return parseTokenCount(raw)
}

// parseTokenCount parses a token count with an optional k/K/m/M suffix, as in
// 8192, 128k or 1.5m. Invalid, zero and negative values return 0.
func parseTokenCount(raw string) int {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}

	multiplier := 1.0
	switch s[len(s)-1] {
	case 'k', 'K':
		multiplier = 1024
		s = s[:len(s)-1]
	case 'm', 'M':
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	}

	value, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	scaled := value * multiplier
	if scaled > float64(int(^uint(0)>>1)) {
		return 0
	}
	return int(scaled)
}

// quantizationFromFilename extracts a quantization token from a model path.
// The basename is used so a directory named after a quant cannot match.
func quantizationFromFilename(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	match := quantizationPattern.FindStringSubmatch(base)
	if match == nil {
		return ""
	}
	// match[1] is the optional dynamic "UD-" prefix; match[2] is the quant.
	return strings.ToUpper(match[2])
}

// basename returns the final path element, and empty for an empty path.
func basename(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// isGGUFPath reports whether path names a GGUF file. A path containing .gguf
// is accepted because sharded models and URLs may not end in it.
func isGGUFPath(path string) bool {
	return strings.Contains(strings.ToLower(path), ".gguf")
}

// ggufCacheKey identifies one version of a model file. A watch-config reload
// creates new ModelConfig values, but the file is unchanged, so its header is
// not read again.
type ggufCacheKey struct {
	path    string
	modTime int64
	size    int64
}

// maxGGUFCacheEntries bounds the header cache. Clearing it wholesale when it
// fills is cheaper than tracking recency and is harmless: entries are only a
// parsing shortcut.
const maxGGUFCacheEntries = 128

var (
	ggufCache     sync.Map
	ggufCacheSize atomic.Int64
)

// cachedGGUFMetadata returns the parsed header for path, reading it once per
// file version. A failed read is not cached, so a file that appears later is
// picked up on the next config reload.
func cachedGGUFMetadata(path string) (modelmeta.Metadata, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return modelmeta.Metadata{}, false
	}

	key := ggufCacheKey{path: path, modTime: info.ModTime().UnixNano(), size: info.Size()}
	if cached, ok := ggufCache.Load(key); ok {
		return cached.(modelmeta.Metadata), true
	}

	meta, err := modelmeta.ReadFile(path)
	if err != nil {
		return modelmeta.Metadata{}, false
	}

	if ggufCacheSize.Add(1) > maxGGUFCacheEntries {
		ggufCache.Range(func(entryKey, _ any) bool {
			ggufCache.Delete(entryKey)
			return true
		})
		ggufCacheSize.Store(0)
	}
	ggufCache.Store(key, meta)
	ggufCacheSize.Add(1)
	return meta, true
}
