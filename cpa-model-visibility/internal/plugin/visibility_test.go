package plugin

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		value, pattern string
		want           bool
	}{
		{"gpt-5.5", "gpt-5.5", true},
		{"gpt-5.5", "GPT-5.5", true},  // case-insensitive
		{"gpt-5.5", "gpt-5.6", false}, // exact only, no prefix bleeding
		{"gpt-5.5-codex", "gpt-5.5", false},
		{"gpt-image-2", "gpt-*", true},
		{"glm-5.3", "gpt-*", false},
		{"codex-auto-review", "codex-*", true},
		{"claude-opus-5-5-high", "*-high", true},
		{"claude-opus-5-5-high", "claude-*-high", true},
		{"claude-opus-5-5-low", "claude-*-high", false},
		{"anything", "*", true},
		{"openrouter/free", "openrouter/free", true},
	}
	for _, tc := range cases {
		if got := matchPattern(tc.value, tc.pattern); got != tc.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.value, tc.pattern, got, tc.want)
		}
	}
}

func TestCallerKey(t *testing.T) {
	if got := callerKey(map[string][]string{"Authorization": {"Bearer sk-test-1"}}); got != "sk-test-1" {
		t.Errorf("bearer key = %q", got)
	}
	if got := callerKey(map[string][]string{"Authorization": {"bearer sk-test-1"}}); got != "sk-test-1" {
		t.Errorf("lowercase bearer = %q", got)
	}
	if got := callerKey(map[string][]string{"X-Api-Key": {"sk-claude"}}); got != "sk-claude" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := callerKey(map[string][]string{"Authorization": {""}, "X-Goog-Api-Key": {"sk-gemini"}}); got != "sk-gemini" {
		t.Errorf("x-goog fallback = %q", got)
	}
	if got := callerKey(nil); got != "" {
		t.Errorf("nil headers = %q", got)
	}
}

const sampleCatalog = `{"data":[` +
	`{"id":"gpt-5.5","object":"model","created":1704067200,"owned_by":"openai"},` +
	`{"id":"gpt-image-2","object":"model","owned_by":"openai"},` +
	`{"id":"glm-5.3","object":"model","created":1704067200,"owned_by":"workbuddy"}` +
	`],"object":"list"}`

func filteredIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if parsed.Object != "list" {
		t.Fatalf("object = %q, want list", parsed.Object)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestFilterModelListRule(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("default: empty\nrules:\n  - keys: [sk-a]\n    deny: [\"gpt-*\"]\n  - keys: [sk-b]\n    allow: [\"glm-*\"]\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// sk-a: codex models hidden by id pattern.
	got := mv.filterModelList(&ResponseInterceptRequest{
		StatusCode:     200,
		RequestHeaders: map[string][]string{"Authorization": {"Bearer sk-a"}},
		Body:           []byte(sampleCatalog),
	})
	if got == nil {
		t.Fatal("expected a filtered body for sk-a")
	}
	if ids := filteredIDs(t, got); !reflect.DeepEqual(ids, []string{"glm-5.3"}) {
		t.Fatalf("sk-a sees %v, want [glm-5.3]", ids)
	}

	// sk-b: allowlist mode, gpt models dropped because they are not allowed.
	got = mv.filterModelList(&ResponseInterceptRequest{
		StatusCode:     200,
		RequestHeaders: map[string][]string{"X-Api-Key": {"sk-b"}},
		Body:           []byte(sampleCatalog),
	})
	if ids := filteredIDs(t, got); !reflect.DeepEqual(ids, []string{"glm-5.3"}) {
		t.Fatalf("sk-b sees %v, want [glm-5.3]", ids)
	}
}

func TestFilterModelListDefaultEmpty(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("default: empty\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := mv.filterModelList(&ResponseInterceptRequest{
		StatusCode:     200,
		RequestHeaders: map[string][]string{"Authorization": {"Bearer sk-unknown"}},
		Body:           []byte(sampleCatalog),
	})
	if got == nil {
		t.Fatal("expected an emptied catalog for an unknown key")
	}
	if ids := filteredIDs(t, got); len(ids) != 0 {
		t.Fatalf("unknown key sees %v, want empty", ids)
	}
}

func TestFilterModelListDefaultFull(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("default: full\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := mv.filterModelList(&ResponseInterceptRequest{
		StatusCode:     200,
		RequestHeaders: map[string][]string{"Authorization": {"Bearer sk-unknown"}},
		Body:           []byte(sampleCatalog),
	}); got != nil {
		t.Fatalf("default full should pass through, got %s", got)
	}
}

func TestFilterModelListPassThroughNonCatalog(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("default: empty\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	bodies := []string{
		`{"choices":[{"message":{"role":"assistant"}}]}`,
		`{"input_tokens":12}`,
		`{"error":{"message":"upstream exploded"}}`,
		`{"data":[{"choices":[]}]}`,         // data but no object:list
		`{"data":"scalar","object":"list"}`, // data not an array
	}
	for _, body := range bodies {
		if got := mv.filterModelList(&ResponseInterceptRequest{StatusCode: 200, Body: []byte(body)}); got != nil {
			t.Fatalf("body %s should pass through, got %s", body, got)
		}
	}
	if got := mv.filterModelList(&ResponseInterceptRequest{StatusCode: 502, Body: []byte(sampleCatalog)}); got != nil {
		t.Fatal("non-200 responses must pass through")
	}
}

func TestFilterModelListKeepsForeignFields(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("default: empty\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := mv.filterModelList(&ResponseInterceptRequest{
		StatusCode: 200,
		Body:       []byte(`{"data":[{"id":"gpt-5.5","object":"model"}],"object":"list","total":3}`),
	})
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if _, ok := parsed["total"]; !ok {
		t.Fatalf("foreign top-level field lost: %s", got)
	}
}

func TestFilterModelListOwnedBy(t *testing.T) {
	mv := newModelVisibility()
	if err := mv.configure([]byte("rules:\n  - keys: [sk-a]\n    deny_owned_by: [openai]\n")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := mv.filterModelList(&ResponseInterceptRequest{
		StatusCode:     200,
		RequestHeaders: map[string][]string{"Authorization": {"Bearer sk-a"}},
		Body:           []byte(sampleCatalog),
	})
	if ids := filteredIDs(t, got); !reflect.DeepEqual(ids, []string{"glm-5.3"}) {
		t.Fatalf("owned_by deny left %v, want [glm-5.3]", ids)
	}
}

func TestConfigureRejectsBadRules(t *testing.T) {
	for _, cfg := range []string{
		"default: sometimes\n",                         // bad policy
		"rules:\n  - deny: [\"x*\"]\n",                 // no keys
		"rules:\n  - keys: [a]\n  - keys: [a]\n",       // duplicate key
		"rules:\n  - keys: [a]\n    deny: [\"a**\"]\n", // double wildcard
		"typo_field: 1\n",                              // unknown field
	} {
		mv := newModelVisibility()
		if err := mv.configure([]byte(cfg)); err == nil {
			t.Errorf("configure(%q) should have failed", cfg)
		}
	}
}
