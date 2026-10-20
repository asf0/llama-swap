package config

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	MODEL_CONFIG_DEFAULT_TTL   = -1
	MODEL_CONFIG_DEFAULT_PROXY = "http://localhost:${PORT}"
	comfyUIConcurrencyLimit    = 999

	// ComfyUIModelID identifies the model used by the /comfyui endpoint.
	ComfyUIModelID = "comfyui_auto"

	// AudioCppModelID identifies the model used by the /audiocpp endpoint.
	AudioCppModelID = "audiocpp_auto"
)

var validModalities = map[string]struct{}{
	"text":  {},
	"audio": {},
	"image": {},
	"video": {},
}

// validOutModalities adds "decisions", the only output of llama.cpp decision
// models, which answer /v1/systemone and generate no text.
var validOutModalities = map[string]struct{}{
	"text":      {},
	"audio":     {},
	"image":     {},
	"video":     {},
	"decisions": {},
}

// ModelCapConfig defines what modalities and features a model supports.
// Used in /v1/models to inform clients. An empty block (all zero values) is
// treated as not configured.
type ModelCapConfig struct {
	In       []string `yaml:"in" json:"in,omitempty"`
	Out      []string `yaml:"out" json:"out,omitempty"`
	Tools    bool     `yaml:"tools" json:"tools,omitempty"`
	Reranker bool     `yaml:"reranker" json:"reranker,omitempty"`
	Context  int      `yaml:"context" json:"context,omitempty"`

	// Voices, when set, is returned as the response to
	// GET /v1/audio/voices?model={id} instead of asking the upstream. It is
	// never discovered and never rendered into /v1/models, so Empty and Merge
	// leave it alone.
	Voices []string `yaml:"voices" json:"-"`

	// DisableAuto turns off automatic capability discovery for this model.
	// It is a switch, not a capability, so Empty ignores it and it is never
	// rendered into /v1/models.
	DisableAuto bool `yaml:"disableAuto" json:"-"`
}

// Empty returns true when all capability fields are at their zero values.
// DisableAuto is deliberately excluded: a block that only turns discovery off
// still advertises nothing, so it must render as if it were absent.
func (c ModelCapConfig) Empty() bool {
	return len(c.In) == 0 && len(c.Out) == 0 && !c.Tools && !c.Reranker && c.Context == 0
}

// Merge returns c with every field left at its zero value filled in from
// auto. Configured values always win, field by field, so a model that sets
// only capabilities.tools still picks up a discovered context length.
//
// A field explicitly set to its zero value in the config is indistinguishable
// from an omitted one, so `tools: false` cannot override a discovered true.
// Use capabilities.disableAuto to suppress discovery for the model instead.
//
// DisableAuto is carried over from c unchanged; auto never sets it.
func (c ModelCapConfig) Merge(auto ModelCapConfig) ModelCapConfig {
	merged := c
	if len(merged.In) == 0 {
		merged.In = auto.In
	}
	if len(merged.Out) == 0 {
		merged.Out = auto.Out
	}
	if !merged.Tools {
		merged.Tools = auto.Tools
	}
	if !merged.Reranker {
		merged.Reranker = auto.Reranker
	}
	if merged.Context == 0 {
		merged.Context = auto.Context
	}
	return merged
}

// Validate checks that all modality values are recognized and context is
// non-negative. Returns an error if any value is invalid.
func (c ModelCapConfig) Validate() error {
	for _, m := range c.In {
		if _, ok := validModalities[m]; !ok {
			return fmt.Errorf("capabilities.in: invalid modality %q, must be one of: text, audio, image, video", m)
		}
	}
	for _, m := range c.Out {
		if _, ok := validOutModalities[m]; !ok {
			return fmt.Errorf("capabilities.out: invalid modality %q, must be one of: text, audio, image, video, decisions", m)
		}
	}
	if c.Context < 0 {
		return errors.New("capabilities.context: must be >= 0")
	}
	return nil
}

// TimeoutsConfig holds timeout settings for proxy connections
// 0 = no timeout
type TimeoutsConfig struct {
	Connect        int `yaml:"connect"`
	KeepAlive      int `yaml:"keepalive"`
	ResponseHeader int `yaml:"responseHeader"`
	TLSHandshake   int `yaml:"tlsHandshake"`
	ExpectContinue int `yaml:"expectContinue"`
	IdleConn       int `yaml:"idleConn"`
}

// CompatConfig holds compatibility settings for upstream applications.
type CompatConfig struct {
	// IgnoreWebsockets prevents websocket connections from participating in
	// model lifecycle activity such as swapping, concurrency, and TTL tracking.
	IgnoreWebsockets bool `yaml:"ignoreWebsockets"`
}

// maxCheckReadyBodyLen is the largest health check body inspected by a
// checkReady predicate, mirroring the progress parser's limit. A readiness
// document is small; anything much bigger is not one.
const maxCheckReadyBodyLen = 64 * 1024

// CheckReadyConfig is an optional predicate applied to a health check response
// whose HTTP status is already 200. It lets an upstream that answers 200 while
// still loading report readiness in its body instead of its status code.
//
// Exactly one of JSONPath and BodyRegex must be set.
type CheckReadyConfig struct {
	// JSONPath is a gjson path evaluated against the response body. With no
	// Equals or NotEquals the path must exist and be neither null nor false,
	// which is how "field present and non-null" is expressed.
	JSONPath string `yaml:"jsonPath" json:"jsonPath,omitempty"`
	// Equals requires the JSONPath value's string form to match this scalar.
	// A null expected value is not supported: YAML cannot distinguish an
	// absent key from an explicit null.
	Equals any `yaml:"equals" json:"equals,omitempty"`
	// NotEquals requires the JSONPath value's string form to differ from this
	// scalar. It may be null.
	NotEquals any `yaml:"notEquals" json:"notEquals,omitempty"`
	// BodyRegex makes readiness depend on a regular expression matching the
	// raw response body. It is an alternative to JSONPath.
	BodyRegex string `yaml:"bodyRegex" json:"bodyRegex,omitempty"`

	regex *regexp.Regexp
}

// UnmarshalYAML compiles bodyRegex while decoding so an invalid expression is
// reported as a config error instead of failing silently at runtime.
func (c *CheckReadyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawCheckReady CheckReadyConfig
	var raw rawCheckReady
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if raw.BodyRegex != "" {
		re, err := regexp.Compile(raw.BodyRegex)
		if err != nil {
			return fmt.Errorf("checkReady.bodyRegex: invalid regular expression %q: %w", raw.BodyRegex, err)
		}
		raw.regex = re
	}
	*c = CheckReadyConfig(raw)
	return nil
}

// Matches reports whether body satisfies the predicate. A nil config always
// matches, so callers can use it without a separate nil check.
func (c *CheckReadyConfig) Matches(body []byte) bool {
	if c == nil {
		return true
	}
	if len(body) > maxCheckReadyBodyLen {
		return false
	}
	if c.regex != nil {
		return c.regex.Match(body)
	}
	if c.JSONPath == "" {
		return false
	}
	// JSONPath-style "$.model" is the documented spelling. gjson paths are
	// relative to the root, so drop the leading "$" and separator if present.
	path := strings.TrimPrefix(c.JSONPath, "$")
	path = strings.TrimPrefix(path, ".")
	result := gjson.GetBytes(body, path)
	if !result.Exists() {
		return false
	}
	if c.Equals == nil && c.NotEquals == nil {
		return result.Type != gjson.Null && result.Type != gjson.False
	}
	if c.Equals != nil {
		return result.String() == fmt.Sprint(c.Equals)
	}
	return result.String() != fmt.Sprint(c.NotEquals)
}

type ModelConfig struct {
	Cmd           string   `yaml:"cmd"`
	CmdStop       string   `yaml:"cmdStop"`
	Proxy         string   `yaml:"proxy"`
	Aliases       []string `yaml:"aliases"`
	Env           []string `yaml:"env"`
	CheckEndpoint string   `yaml:"checkEndpoint"`
	UnloadAfter   int      `yaml:"ttl"`
	UnloadTimeout int      `yaml:"unloadTimeout"`
	Unlisted      bool     `yaml:"unlisted"`
	UseModelName  string   `yaml:"useModelName"`

	// #179 for /v1/models
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Limit concurrency of HTTP requests to process
	ConcurrencyLimit int `yaml:"concurrencyLimit"`

	// Model filters see issue #174
	Filters ModelFilters `yaml:"filters"`

	// Macros: see #264
	// Model level macros take precedence over the global macros
	Macros MacroList `yaml:"macros"`

	// Metadata: see #264
	// Arbitrary metadata that can be exposed through the API
	Metadata map[string]any `yaml:"metadata"`

	// override global setting
	SendLoadingState *bool `yaml:"sendLoadingState"`

	// Timeout settings for proxy connections
	Timeouts TimeoutsConfig `yaml:"timeouts"`

	// DisableKeepAlives opens a new upstream connection for every request
	// instead of reusing pooled ones. Works around upstreams that advertise
	// keep-alive and then close the connection (#1205).
	DisableKeepAlives bool `yaml:"disableKeepAlives"`

	// Compatibility settings for upstream applications.
	Compat CompatConfig `yaml:"compat"`

	// Capabilities defines what modalities and features the model supports.
	Capabilities ModelCapConfig `yaml:"capabilities"`

	// HealthCheckTimeout overrides the global healthCheckTimeout for this
	// model when non-zero. Config loading fills in the global value and clamps
	// the effective value to the 15-second minimum.
	HealthCheckTimeout int `yaml:"healthCheckTimeout"`

	// CheckReady adds a body predicate to a 200 health check response.
	CheckReady *CheckReadyConfig `yaml:"checkReady"`
}

func (m *ModelConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelConfig ModelConfig
	defaults := rawModelConfig{
		Cmd:              "",
		CmdStop:          "",
		Proxy:            MODEL_CONFIG_DEFAULT_PROXY,
		Aliases:          []string{},
		Env:              []string{},
		CheckEndpoint:    "/health",
		UnloadAfter:      MODEL_CONFIG_DEFAULT_TTL, // use GlobalTTL
		UnloadTimeout:    0,                        // use global UnloadTimeout
		Unlisted:         false,
		UseModelName:     "",
		ConcurrencyLimit: 0,
		Name:             "",
		Description:      "",

		// matches http.DefaultTransport
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 0,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	// the default cmdStop to taskkill /f /t /pid ${PID}
	if runtime.GOOS == "windows" {
		defaults.CmdStop = "taskkill /f /t /pid ${PID}"
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*m = ModelConfig(defaults)
	return nil
}

func (m *ModelConfig) SanitizedCommand() ([]string, error) {
	return SanitizeCommand(m.Cmd)
}

// ModelFilters embeds Filters and adds legacy support for strip_params field
// See issue #174
type ModelFilters struct {
	Filters `yaml:",inline"`
}

func (m *ModelFilters) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelFilters ModelFilters
	defaults := rawModelFilters{}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	// Try to unmarshal with the old field name for backwards compatibility
	if defaults.StripParams == "" {
		var legacy struct {
			StripParams string `yaml:"strip_params"`
		}
		if legacyErr := unmarshal(&legacy); legacyErr != nil {
			return errors.New("failed to unmarshal legacy filters.strip_params: " + legacyErr.Error())
		}
		defaults.StripParams = legacy.StripParams
	}

	*m = ModelFilters(defaults)
	return nil
}

// SanitizedStripParams wraps Filters.SanitizedStripParams for backwards compatibility
// Returns ([]string, error) to match existing API
func (f ModelFilters) SanitizedStripParams() ([]string, error) {
	return f.Filters.SanitizedStripParams(), nil
}
