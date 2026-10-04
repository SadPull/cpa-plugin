package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// DefaultPolicy names what a key without a matching rule sees.
const (
	DefaultEmpty = "empty" // data: [] (nothing visible)
	DefaultFull  = "full"  // the unfiltered catalog (pass-through)
)

// Config is the plugins.configs.<PluginID> subtree delivered by the host.
// The host injects enabled/priority scalars into the YAML it forwards; they
// are declared here so strict decoding tolerates them.
type Config struct {
	Enabled  bool         `yaml:"enabled"`
	Priority int          `yaml:"priority"`
	Debug    bool         `yaml:"debug"`
	Default  string       `yaml:"default"`
	Rules    []RuleConfig `yaml:"rules"`
}

// RuleConfig restricts one or more API keys.
//
// Matching is case-insensitive. Model patterns follow the
// oauth-excluded-models dialect: "*" matches everything, "abc*" is a prefix,
// "*abc" a suffix, "a*bc" anchored on both ends; a pattern without "*" is an
// exact match. An empty allow list means "no restriction on that dimension";
// deny always wins over allow. The first rule whose keys contain the caller
// wins; a key must not appear in two rules.
type RuleConfig struct {
	Keys         []string `yaml:"keys"`
	Allow        []string `yaml:"allow"`
	Deny         []string `yaml:"deny"`
	AllowOwnedBy []string `yaml:"allow_owned_by"`
	DenyOwnedBy  []string `yaml:"deny_owned_by"`
}

// compiledConfig is the immutable snapshot swapped in on register/reconfigure.
type compiledConfig struct {
	rules  []RuleConfig
	byKey  map[string]*RuleConfig
	policy string
	debug  bool
}

type modelVisibility struct {
	state atomic.Pointer[compiledConfig]
}

func newModelVisibility() *modelVisibility {
	mv := &modelVisibility{}
	mv.state.Store(&compiledConfig{policy: DefaultEmpty, byKey: map[string]*RuleConfig{}})
	return mv
}

// configure parses the plugins.configs subtree delivered by the host and
// swaps the active snapshot atomically. Unknown YAML fields are rejected so
// typos fail loudly instead of silently matching nothing.
func (mv *modelVisibility) configure(configYAML []byte) error {
	cfg := Config{Default: DefaultEmpty}
	trimmed := bytes.TrimSpace(configYAML)
	if len(trimmed) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(trimmed))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
	}
	compiled, err := compileConfig(cfg)
	if err != nil {
		return err
	}
	mv.state.Store(compiled)
	return nil
}

func compileConfig(cfg Config) (*compiledConfig, error) {
	compiled := &compiledConfig{
		rules:  cfg.Rules,
		byKey:  make(map[string]*RuleConfig, len(cfg.Rules)),
		policy: strings.ToLower(strings.TrimSpace(cfg.Default)),
		debug:  cfg.Debug,
	}
	switch compiled.policy {
	case "", DefaultEmpty:
		compiled.policy = DefaultEmpty
	case DefaultFull:
	default:
		return nil, fmt.Errorf("default must be %q or %q", DefaultEmpty, DefaultFull)
	}
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		if len(rule.Keys) == 0 {
			return nil, fmt.Errorf("rule #%d has no keys", i+1)
		}
		for _, key := range rule.Keys {
			key = strings.TrimSpace(key)
			if key == "" {
				return nil, fmt.Errorf("rule #%d has an empty key", i+1)
			}
			if _, dup := compiled.byKey[key]; dup {
				return nil, fmt.Errorf("key %s appears in more than one rule", previewKey(key))
			}
			compiled.byKey[key] = rule
		}
		for _, group := range [][]string{rule.Allow, rule.Deny, rule.AllowOwnedBy, rule.DenyOwnedBy} {
			for _, pattern := range group {
				if err := validatePattern(pattern); err != nil {
					return nil, err
				}
			}
		}
	}
	return compiled, nil
}

func (mv *modelVisibility) snapshot() *compiledConfig { return mv.state.Load() }

// validatePattern rejects patterns that can never match anything useful.
func validatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("empty model pattern")
	}
	if len(pattern) > 512 {
		return fmt.Errorf("model pattern too long")
	}
	if strings.Count(pattern, "*") > 1 {
		return fmt.Errorf("model pattern %q: only one wildcard is supported", pattern)
	}
	return nil
}

// matchPattern reports whether value matches pattern (case-insensitive):
// exact, "abc*" prefix, "*abc" suffix, "a*bc" anchored on both ends, or "*".
func matchPattern(value, pattern string) bool {
	value = strings.ToLower(value)
	pattern = strings.ToLower(pattern)
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return value == pattern
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if len(value) < len(prefix)+len(suffix) {
		return false
	}
	return strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix)
}

func matchAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchPattern(value, pattern) {
			return true
		}
	}
	return false
}

// visible applies one rule to a catalog entry. Deny wins over allow.
func (r *RuleConfig) visible(id, ownedBy string) bool {
	if matchAny(id, r.Deny) || matchAny(ownedBy, r.DenyOwnedBy) {
		return false
	}
	if len(r.Allow) > 0 && !matchAny(id, r.Allow) {
		return false
	}
	if len(r.AllowOwnedBy) > 0 && !matchAny(ownedBy, r.AllowOwnedBy) {
		return false
	}
	return true
}

// callerKey extracts the downstream API key from request headers.
// AuthMiddleware has already validated it; this only re-identifies the caller.
func callerKey(headers map[string][]string) string {
	if headers == nil {
		return ""
	}
	first := func(name string) string {
		for _, v := range headers[name] {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return ""
	}
	if auth := first("Authorization"); auth != "" {
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
			auth = strings.TrimSpace(auth[7:])
		}
		if auth != "" {
			return auth
		}
	}
	if key := first("X-Api-Key"); key != "" {
		return key
	}
	return first("X-Goog-Api-Key")
}

// previewKey masks a key for logs and management output.
func previewKey(key string) string {
	if len(key) <= 10 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// filterModelList applies the per-key rules to a model-list response body.
//
// The response.intercept_after hook fires for every non-streaming response,
// not just catalog listings, so the body shape is verified before touching
// anything: top-level {"data":[...],"object":"list"} where every entry is an
// object carrying a string "id" (and, when present, object=="model").
// Anything else — chat completions, count_tokens, errors — passes through
// unchanged. All failures are fail-open by design: an over-visible catalog
// is harmless (request-level enforcement still applies), a mangled response
// is not.
//
// Returns nil when the body should be left untouched; the host replaces the
// body only when the plugin returns a non-empty one.
func (mv *modelVisibility) filterModelList(req *ResponseInterceptRequest) []byte {
	if req.StatusCode != 200 || len(req.Body) == 0 {
		return nil
	}
	body := bytes.TrimSpace(req.Body)
	if len(body) == 0 || body[0] != '{' || !bytes.Contains(body, []byte(`"object":"list"`)) {
		return nil
	}

	list, ok := parseModelList(body)
	if !ok {
		return nil
	}

	snap := mv.snapshot()
	key := callerKey(req.RequestHeaders)
	rule, hasRule := snap.byKey[key]

	kept := list.entries
	removed := 0
	switch {
	case hasRule:
		kept = make([]listEntry, 0, len(list.entries))
		for _, entry := range list.entries {
			if rule.visible(entry.id, entry.ownedBy) {
				kept = append(kept, entry)
			} else {
				removed++
			}
		}
	case snap.policy == DefaultFull:
		return nil
	default: // DefaultEmpty: an unrestricted key sees nothing
		removed = len(list.entries)
		kept = nil
	}

	if removed == 0 {
		return nil // nothing filtered; keep the original bytes untouched
	}

	out, err := rebuildModelList(list, kept)
	if err != nil {
		return nil
	}
	if snap.debug {
		hostLog(req.HostCallbackID, "debug", "filtered model catalog",
			map[string]any{
				"key":     previewKey(key),
				"matched": hasRule,
				"kept":    len(kept),
				"hidden":  removed,
			})
	}
	return out
}

type listEntry struct {
	id      string
	ownedBy string
	raw     json.RawMessage
}

type modelList struct {
	top     map[string]json.RawMessage
	entries []listEntry
}

// parseModelList validates the catalog shape and extracts entries.
func parseModelList(body []byte) (modelList, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return modelList{}, false
	}
	if obj, ok := top["object"]; !ok || string(bytes.TrimSpace(obj)) != `"list"` {
		return modelList{}, false
	}
	data, ok := top["data"]
	if !ok {
		return modelList{}, false
	}
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(data, &rawEntries); err != nil {
		return modelList{}, false
	}
	entries := make([]listEntry, 0, len(rawEntries))
	for _, raw := range rawEntries {
		var item struct {
			ID      *string `json:"id"`
			Object  *string `json:"object"`
			OwnedBy *string `json:"owned_by"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.ID == nil || *item.ID == "" {
			return modelList{}, false
		}
		if item.Object != nil && *item.Object != "model" {
			return modelList{}, false
		}
		ownedBy := ""
		if item.OwnedBy != nil {
			ownedBy = *item.OwnedBy
		}
		entries = append(entries, listEntry{id: *item.ID, ownedBy: ownedBy, raw: raw})
	}
	return modelList{top: top, entries: entries}, true
}

// rebuildModelList reassembles the catalog with the filtered data array,
// preserving every other top-level field.
func rebuildModelList(list modelList, kept []listEntry) ([]byte, error) {
	raws := make([]json.RawMessage, len(kept))
	for i, entry := range kept {
		raws[i] = entry.raw
	}
	data, err := json.Marshal(raws)
	if err != nil {
		return nil, err
	}
	list.top["data"] = data
	return json.Marshal(list.top)
}
