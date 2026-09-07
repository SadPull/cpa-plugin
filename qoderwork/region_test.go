package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeRegion(t *testing.T) {
	cases := map[string]string{
		"":             RegionGlobal,
		"global":       RegionGlobal,
		"GLOBAL":       RegionGlobal,
		"qoder.sh":     RegionGlobal,
		"qoder.com":    RegionGlobal,
		"cn":           RegionCN,
		"CN":           RegionCN,
		"qoder.com.cn": RegionCN,
		"nonsense":     RegionGlobal,
	}
	for in, want := range cases {
		if got := normalizeRegion(in, RegionGlobal); got != want {
			t.Fatalf("normalizeRegion(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeRegion("nonsense", RegionCN); got != RegionCN {
		t.Fatalf("normalizeRegion fallback def: got %q want cn", got)
	}
}

func TestRegionForAuth(t *testing.T) {
	if got := regionForAuth(nil); got != defaultRegion() {
		t.Fatalf("nil auth should resolve to default, got %q", got)
	}
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-x"}}
	if got := regionForAuth(sa); got != RegionGlobal {
		t.Fatalf("empty region should fall back to default(global), got %q", got)
	}
	sa.Auth.Region = RegionCN
	if got := regionForAuth(sa); got != RegionCN {
		t.Fatalf("explicit region, got %q want cn", got)
	}
	sa.Auth.Region = ""
	sa.Auth.Domain = "qoder.com.cn"
	if got := regionForAuth(sa); got != RegionCN {
		t.Fatalf("legacy domain should map to cn, got %q", got)
	}
	sa.Auth.Domain = "qoder.sh"
	if got := regionForAuth(sa); got != RegionGlobal {
		t.Fatalf("legacy domain should map to global, got %q", got)
	}
}

func TestRegionSpecs(t *testing.T) {
	g := specFor(RegionGlobal)
	if g.ChatURL != "https://api2-v2.qoder.sh/model/v1/chat/completions" {
		t.Fatalf("global chat url: %q", g.ChatURL)
	}
	if !strings.HasPrefix(g.OpenAPIBase, "https://openapi.qoder.sh") {
		t.Fatalf("global openapi base: %q", g.OpenAPIBase)
	}
	c := specFor(RegionCN)
	if !strings.HasPrefix(c.ChatURL, "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation") {
		t.Fatalf("cn chat url: %q", c.ChatURL)
	}
	if c.ClientID == "" || g.ClientID == "" {
		t.Fatal("both realms need a device-flow client_id")
	}
}

func TestParseStoredRegionVariants(t *testing.T) {
	// Nested shape with explicit region.
	nested := `{"auth":{"accessToken":"dt-x","refreshToken":"drt-y","region":"global"},"account":{"uid":"u1","nickname":"n1"}}`
	sa, err := parseStored([]byte(nested))
	if err != nil {
		t.Fatal(err)
	}
	if sa.Auth.Region != RegionGlobal {
		t.Fatalf("nested region: got %q", sa.Auth.Region)
	}
	// Flat shape with legacy domain only.
	flat := `{"accessToken":"dt-x","refreshToken":"drt-y","domain":"qoder.com.cn","uid":"u2","nickname":"n2"}`
	sa2, err := parseStored([]byte(flat))
	if err != nil {
		t.Fatal(err)
	}
	if got := regionForAuth(sa2); got != RegionCN {
		t.Fatalf("flat+domain should resolve cn, got %q", got)
	}
}

func TestBuildGlobalBody(t *testing.T) {
	payload := `{"model":"qwen3.8-max-preview","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"type":"function","function":{"name":"f"}}],"temperature":0.5}`
	body, err := buildGlobalBody([]byte(payload), "qmodel_preview")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "qmodel_preview" {
		t.Fatalf("model override: %v", m["model"])
	}
	if m["stream"] != true {
		t.Fatal("stream must be forced true")
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatal("stream_options.include_usage missing")
	}
	meta, _ := m["metadata"].(map[string]any)
	ctx, _ := meta["context"].(map[string]any)
	if ctx == nil || ctx["client_type"] != "qodercli" || ctx["task_id"] != "common" {
		t.Fatalf("metadata.context wrong: %v", ctx)
	}
	if _, ok := ctx["request_id"]; !ok {
		t.Fatal("request_id missing")
	}
	// Client payload preserved: multi-part content + tools + temperature.
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len: %d", len(msgs))
	}
	if _, ok := m["tools"].([]any); !ok {
		t.Fatal("tools not passed through")
	}
	if m["temperature"] != 0.5 {
		t.Fatalf("temperature not passed through: %v", m["temperature"])
	}
}

func TestBuildGlobalBodyEmpty(t *testing.T) {
	body, err := buildGlobalBody(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "lite" {
		t.Fatalf("empty model should default to lite, got %v", m["model"])
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Fatal("messages should default to empty list")
	}
}

func TestUnwrapOpenAIChunk(t *testing.T) {
	if inner, ok, done := unwrapOpenAIChunk([]byte("[DONE]")); !done || ok || inner != "" {
		t.Fatalf("[DONE] should be done-only, got %q ok=%v done=%v", inner, ok, done)
	}
	chunk := `{"choices":[{"delta":{"content":"hi"}}]}`
	inner, ok, done := unwrapOpenAIChunk([]byte(chunk))
	if !ok || done || inner != chunk {
		t.Fatalf("standard chunk passthrough failed: %q ok=%v done=%v", inner, ok, done)
	}
	if _, ok, _ := unwrapOpenAIChunk([]byte("not json")); ok {
		t.Fatal("non-JSON should be skipped")
	}
}

func TestUnwrapCNBody(t *testing.T) {
	inner := `{"choices":[{"delta":{"content":"hi"}}]}`
	wrapped := `{"headers":{},"body":` + quoteJSON(inner) + `,"statusCodeValue":200}`
	got, ok, done := unwrapCNBody([]byte(wrapped))
	if !ok || done || got != inner {
		t.Fatalf("nested unwrap failed: %q ok=%v done=%v", got, ok, done)
	}
	if _, ok, done := unwrapCNBody([]byte(`{"body":"[DONE]"}`)); ok || !done {
		t.Fatal("CN [DONE] must terminate")
	}
	if _, ok, _ := unwrapCNBody([]byte(`{"noBody":1}`)); ok {
		t.Fatal("missing body must be skipped")
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestGlobalModelKey(t *testing.T) {
	if got := globalModelKey(""); got != "lite" {
		t.Fatalf("empty → lite, got %q", got)
	}
	if got := globalModelKey("qwen3.8-max-preview"); got != "qmodel_preview" {
		t.Fatalf("friendly name mapping, got %q", got)
	}
	if got := globalModelKey("qmodel_preview"); got != "qmodel_preview" {
		t.Fatalf("passthrough, got %q", got)
	}
}

func TestRegionDomain(t *testing.T) {
	if got := regionDomain(RegionCN); got != "qoder.com.cn" {
		t.Fatalf("cn domain: %q", got)
	}
	if got := regionDomain(RegionGlobal); got != "qoder.sh" {
		t.Fatalf("global domain: %q", got)
	}
}
