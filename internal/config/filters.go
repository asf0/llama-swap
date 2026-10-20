package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ProtectedParams is a list of parameters that cannot be set or stripped via filters
// These are protected to prevent breaking the proxy's ability to route requests correctly
var ProtectedParams = []string{"model"}

// Filters contains filter settings for modifying request parameters
// Used by both models and peers
type Filters struct {
	// StripParams is a comma-separated list of parameters to remove from requests
	// The "model" parameter can never be removed
	StripParams string `yaml:"stripParams"`

	// SetParams is a dictionary of parameters to set/override in requests.
	// A key ending in "?" (e.g. "max_tokens?") is set-if-undefined: it is only
	// applied when the request does not already carry that parameter.
	// Protected params (like "model") cannot be set
	SetParams map[string]any `yaml:"setParams"`

	// SetParamsByID maps requested model IDs to parameters to set/override in requests.
	// Useful with aliases: a single loaded model can behave differently depending on
	// which alias the client used. Applied after SetParams, so it can override those values.
	// Keys ending in "?" are set-if-undefined, as in SetParams.
	// Protected params (like "model") cannot be set.
	SetParamsByID map[string]map[string]any `yaml:"setParamsByID"`

	// SetParamsByMatch applies parameters when a request field equals a value.
	// Rules are evaluated in order and every matching rule applies, so a later
	// rule overrides an earlier one. Applied after StripParams and before
	// SetParams/SetParamsByID, which keep the final say.
	SetParamsByMatch []MatchRule `yaml:"setParamsByMatch"`
}

// MatchRule sets request parameters when a request field equals a specific
// value. Unlike SetParamsByID, which keys off the requested model ID, a rule
// matches on the contents of the request body. Clients can therefore vary
// parameters between requests without changing the model they ask for, so no
// model swap is triggered and the loaded process is reused.
type MatchRule struct {
	// Key is the top-level request field to inspect (e.g. "reasoning_effort").
	Key string `yaml:"key"`

	// Match is the value Key must equal for the rule to apply. Non-string JSON
	// values are compared by their string form, so YAML booleans and numbers
	// must be quoted to match them.
	Match string `yaml:"match"`

	// Set holds the parameters applied when the rule matches. A value that is
	// a map is merged into an existing request object rather than replacing it.
	// Protected params (like "model") cannot be set.
	Set map[string]any `yaml:"set"`
}

// matchRuleKeyRegex restricts a rule key to characters gjson treats literally,
// so a lookup always targets the same top-level field.
var matchRuleKeyRegex = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Validate checks a single match rule at config load time.
func (r MatchRule) Validate() error {
	if r.Key == "" {
		return fmt.Errorf("key must not be empty")
	}
	if slices.Contains(ProtectedParams, r.Key) {
		return fmt.Errorf("key '%s' is a protected parameter", r.Key)
	}
	if !matchRuleKeyRegex.MatchString(r.Key) {
		return fmt.Errorf("key '%s' must contain only letters, digits, underscores, or hyphens", r.Key)
	}
	if len(r.Set) == 0 {
		return fmt.Errorf("set must not be empty")
	}
	return nil
}

// SanitizedSet returns the rule's params with protected params removed and keys
// sorted for consistent iteration order. Mirrors SanitizedSetParams.
func (r MatchRule) SanitizedSet() (map[string]any, []string) {
	if len(r.Set) == 0 {
		return nil, nil
	}

	result := make(map[string]any, len(r.Set))
	keys := make([]string, 0, len(r.Set))

	for key, value := range r.Set {
		if slices.Contains(ProtectedParams, key) {
			continue
		}
		result[key] = value
		keys = append(keys, key)
	}

	sort.Strings(keys)

	if len(result) == 0 {
		return nil, nil
	}

	return result, keys
}

// Merge returns the effective filters for a local model. f holds the
// top-level defaults and override holds the model's own filters. The receiver
// and override are not mutated.
//
// stripParams lists are combined; setParams maps are merged recursively with
// override winning on shared keys; setParamsByMatch rules are appended after
// the defaults so a model rule wins when both match; setParamsByID entries are
// merged per requested ID with override winning on shared params.
func (f Filters) Merge(override Filters) Filters {
	return Filters{
		StripParams:      mergeStripParams(f.StripParams, override.StripParams),
		SetParams:        mergeAnyMaps(f.SetParams, override.SetParams),
		SetParamsByID:    mergeSetParamsByID(f.SetParamsByID, override.SetParamsByID),
		SetParamsByMatch: mergeMatchRules(f.SetParamsByMatch, override.SetParamsByMatch),
	}
}

// mergeStripParams joins two comma-separated lists. Duplicates, whitespace and
// empty entries are handled later by SanitizedStripParams.
func mergeStripParams(base, override string) string {
	switch {
	case base == "":
		return override
	case override == "":
		return base
	default:
		return base + ", " + override
	}
}

// mergeAnyMaps deep merges override into base. Nested map[string]any values
// merge recursively so an override can replace one nested key without
// discarding its siblings. Any other override value replaces the base value.
func mergeAnyMaps(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		if baseValue, ok := merged[key].(map[string]any); ok {
			if overrideValue, ok := value.(map[string]any); ok {
				merged[key] = mergeAnyMaps(baseValue, overrideValue)
				continue
			}
		}
		merged[key] = value
	}
	return merged
}

// mergeSetParamsByID merges the outer maps and deep-merges the params for IDs
// present in both, with the override's params winning on shared keys.
func mergeSetParamsByID(base, override map[string]map[string]any) map[string]map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	merged := make(map[string]map[string]any, len(base)+len(override))
	for id, params := range base {
		merged[id] = params
	}
	for id, params := range override {
		merged[id] = mergeAnyMaps(merged[id], params)
	}
	return merged
}

// mergeMatchRules appends the override's rules after the base rules. Every
// matching rule applies in order, so later (model) rules win on shared keys.
func mergeMatchRules(base, override []MatchRule) []MatchRule {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	merged := make([]MatchRule, 0, len(base)+len(override))
	merged = append(merged, base...)
	merged = append(merged, override...)
	return merged
}

// SanitizedStripParams returns a sorted list of parameters to strip,
// with duplicates, empty strings, and protected params removed
func (f Filters) SanitizedStripParams() []string {
	if f.StripParams == "" {
		return nil
	}

	params := strings.Split(f.StripParams, ",")
	cleaned := make([]string, 0, len(params))
	seen := make(map[string]bool)

	for _, param := range params {
		trimmed := strings.TrimSpace(param)
		// Skip protected params, empty strings, and duplicates
		if slices.Contains(ProtectedParams, trimmed) || trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}

	if len(cleaned) == 0 {
		return nil
	}

	slices.Sort(cleaned)
	return cleaned
}

// SanitizedSetParamsByID returns the params to set for the given requestedModelID,
// with protected params removed, the "?" set-if-undefined suffix stripped from
// keys, and keys sorted for consistent iteration order. Keys spelled with the
// suffix are reported in soft. Returns nil if the ID has no entry or all its
// params are protected.
func (f Filters) SanitizedSetParamsByID(requestedModelID string) (map[string]any, []string, map[string]bool) {
	if len(f.SetParamsByID) == 0 {
		return nil, nil, nil
	}
	params, found := f.SetParamsByID[requestedModelID]
	if !found || len(params) == 0 {
		return nil, nil, nil
	}
	return sanitizeParams(params)
}

// SanitizedSetParams returns a copy of SetParams with protected params removed,
// the "?" set-if-undefined suffix stripped from keys, and keys sorted for
// consistent iteration order. Keys spelled with the suffix are reported in soft.
func (f Filters) SanitizedSetParams() (map[string]any, []string, map[string]bool) {
	if len(f.SetParams) == 0 {
		return nil, nil, nil
	}
	return sanitizeParams(f.SetParams)
}

// sanitizeParams removes protected params from raw and strips the "?" suffix
// that marks a set-if-undefined key (issue #1052), e.g. "max_tokens?": 32768.
// Such keys are returned (without the suffix) in soft; callers apply them only
// when the body does not already carry the parameter. When the same key is
// spelled both hard and soft, the hard spelling wins. Keys are sorted for
// consistent iteration order.
func sanitizeParams(raw map[string]any) (map[string]any, []string, map[string]bool) {
	result := make(map[string]any, len(raw))
	soft := make(map[string]bool)

	// Hard keys first, so "key" wins over "key?"
	for key, value := range raw {
		if strings.HasSuffix(key, "?") || slices.Contains(ProtectedParams, key) {
			continue
		}
		result[key] = value
	}

	for key, value := range raw {
		if !strings.HasSuffix(key, "?") {
			continue
		}
		key = strings.TrimSuffix(key, "?")
		if key == "" || slices.Contains(ProtectedParams, key) {
			continue
		}
		if _, isHard := result[key]; isHard {
			continue
		}
		result[key] = value
		soft[key] = true
	}

	if len(result) == 0 {
		return nil, nil, nil
	}
	if len(soft) == 0 {
		soft = nil
	}

	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return result, keys, soft
}
