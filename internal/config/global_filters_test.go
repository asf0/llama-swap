package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_GlobalFiltersInherited(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
filters:
  stripParams: "temperature"
  setParams:
    top_p: 0.9
  setParamsByMatch:
    - key: reasoning_effort
      match: low
      set:
        reasoning_budget_tokens: 2048
models:
  inherited:
    cmd: path/to/cmd --port ${PORT}
`))
	require.NoError(t, err)

	filters := cfg.Models["inherited"].Filters
	assert.True(t, filters.InheritsGlobal())
	assert.Equal(t, "temperature", filters.StripParams)
	assert.Equal(t, map[string]any{"top_p": 0.9}, filters.SetParams)
	require.Len(t, filters.SetParamsByMatch, 1)
	assert.Equal(t, "reasoning_effort", filters.SetParamsByMatch[0].Key)
	assert.Equal(t, map[string]any{"reasoning_budget_tokens": 2048}, filters.SetParamsByMatch[0].Set)
}

func TestConfig_GlobalFiltersInheritFalse(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
filters:
  stripParams: "temperature"
  setParams:
    top_p: 0.9
models:
  opted-out:
    cmd: path/to/cmd --port ${PORT}
    filters:
      inherit: false
      setParams:
        top_k: 40
`))
	require.NoError(t, err)

	filters := cfg.Models["opted-out"].Filters
	assert.False(t, filters.InheritsGlobal())
	assert.Empty(t, filters.StripParams)
	assert.Equal(t, map[string]any{"top_k": 40}, filters.SetParams)
	assert.Empty(t, filters.SetParamsByMatch)
}

func TestConfig_GlobalFiltersMerge(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
filters:
  stripParams: "temperature, top_p"
  setParams:
    top_k: 40
    provider:
      data_collection: deny
      allow_fallbacks: true
  setParamsByMatch:
    - key: reasoning_effort
      match: low
      set:
        reasoning_budget_tokens: 1024
        shared: global
  setParamsByID:
    shared-id:
      top_p: 0.5
      nested:
        a: 1
        b: 2
models:
  merged:
    cmd: path/to/cmd --port ${PORT}
    filters:
      stripParams: "top_k"
      setParams:
        provider:
          allow_fallbacks: false
        model_only: true
      setParamsByMatch:
        - key: reasoning_effort
          match: low
          set:
            reasoning_budget_tokens: 2048
            shared: model
      setParamsByID:
        shared-id:
          top_p: 0.9
          nested:
            b: 3
`))
	require.NoError(t, err)

	filters := cfg.Models["merged"].Filters
	stripParams, err := filters.SanitizedStripParams()
	require.NoError(t, err)
	assert.Equal(t, []string{"temperature", "top_k", "top_p"}, stripParams)
	assert.Equal(t, map[string]any{
		"top_k": 40,
		"provider": map[string]any{
			"data_collection": "deny",
			"allow_fallbacks": false,
		},
		"model_only": true,
	}, filters.SetParams)

	require.Len(t, filters.SetParamsByMatch, 2)
	assert.Equal(t, "global", filters.SetParamsByMatch[0].Set["shared"])
	assert.Equal(t, 1024, filters.SetParamsByMatch[0].Set["reasoning_budget_tokens"])
	assert.Equal(t, "model", filters.SetParamsByMatch[1].Set["shared"])
	assert.Equal(t, 2048, filters.SetParamsByMatch[1].Set["reasoning_budget_tokens"])

	assert.Equal(t, map[string]map[string]any{
		"shared-id": {
			"top_p": 0.9,
			"nested": map[string]any{
				"a": 1,
				"b": 3,
			},
		},
	}, filters.SetParamsByID)

	// Merging must not mutate the top-level defaults.
	assert.Equal(t, "temperature, top_p", cfg.Filters.StripParams)
	assert.Equal(t, true, cfg.Filters.SetParams["provider"].(map[string]any)["allow_fallbacks"])
	assert.Equal(t, 0.5, cfg.Filters.SetParamsByID["shared-id"]["top_p"])
	assert.Equal(t, 2, cfg.Filters.SetParamsByID["shared-id"]["nested"].(map[string]any)["b"])
	require.Len(t, cfg.Filters.SetParamsByMatch, 1)
}

func TestConfig_GlobalFiltersMacroExpanded(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
macros:
  reasoning_rules:
    - key: reasoning_effort
      match: low
      set:
        reasoning_budget_tokens: 2048
filters:
  setParamsByMatch: ${reasoning_rules}
models:
  inherited:
    cmd: path/to/cmd --port ${PORT}
`))
	require.NoError(t, err)

	rules := cfg.Models["inherited"].Filters.SetParamsByMatch
	require.Len(t, rules, 1)
	assert.Equal(t, map[string]any{"reasoning_budget_tokens": 2048}, rules[0].Set)
}

func TestConfig_NoGlobalFiltersLeavesModelFiltersUnchanged(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  configured:
    cmd: path/to/cmd --port ${PORT}
    filters:
      stripParams: "top_k"
      setParams:
        temperature: 0.2
  unconfigured:
    cmd: path/to/cmd --port ${PORT}
`))
	require.NoError(t, err)

	configured := cfg.Models["configured"].Filters
	assert.Equal(t, "top_k", configured.StripParams)
	assert.Equal(t, map[string]any{"temperature": 0.2}, configured.SetParams)
	assert.Nil(t, configured.SetParamsByMatch)
	assert.Nil(t, configured.SetParamsByID)

	assert.Equal(t, ModelFilters{}, cfg.Models["unconfigured"].Filters)
}

func TestConfig_GlobalSetParamsByMatchValidation(t *testing.T) {
	_, err := LoadConfigFromReader(strings.NewReader(`
filters:
  setParamsByMatch:
    - key: model
      match: hijacked
      set:
        top_p: 0.1
models:
  m:
    cmd: path/to/cmd --port ${PORT}
`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "filters.setParamsByMatch[0]")
	assert.ErrorContains(t, err, "protected parameter")
}

func TestConfig_GlobalSetParamsByIDNotAutoAlias(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
filters:
  setParamsByID:
    global-variant:
      top_p: 0.5
models:
  inherited:
    cmd: path/to/cmd --port ${PORT}
  with-alias:
    cmd: path/to/cmd --port ${PORT}
    filters:
      setParamsByID:
        with-alias:high:
          top_p: 0.9
`))
	require.NoError(t, err)

	_, found := cfg.RealModelName("global-variant")
	assert.False(t, found, "global setParamsByID keys must not become aliases")
	assert.NotContains(t, cfg.Models["inherited"].Aliases, "global-variant")
	assert.Contains(t, cfg.Models["inherited"].Filters.SetParamsByID, "global-variant")

	realName, found := cfg.RealModelName("with-alias:high")
	require.True(t, found, "model-level setParamsByID keys still become aliases")
	assert.Equal(t, "with-alias", realName)
	assert.Contains(t, cfg.Models["with-alias"].Filters.SetParamsByID, "global-variant")
}

func TestFilters_MergeNilAndEmpty(t *testing.T) {
	model := Filters{
		StripParams: "top_k",
		SetParams:   map[string]any{"temperature": 0.2},
	}

	merged := Filters{}.Merge(model)
	assert.Equal(t, "top_k", merged.StripParams)
	assert.Equal(t, map[string]any{"temperature": 0.2}, merged.SetParams)
	assert.Nil(t, merged.SetParamsByID)
	assert.Nil(t, merged.SetParamsByMatch)

	assert.Equal(t, Filters{}, Filters{}.Merge(Filters{}))
}
