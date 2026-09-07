package main

import (
	"testing"
)

func TestSlugifyDisplayName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max-Preview": "qwen3.8-max-preview",
		"GLM-5.2":             "glm-5.2",
		"Kimi K2.7 Code":      "kimi-k2.7-code",
		"DeepSeek_V4_Pro":     "deepseek-v4-pro",
		"MiniMax-M2.7":        "minimax-m2.7",
		"  Auto  ":            "auto",
		"中文模型":                "",
	}
	for in, want := range cases {
		if got := slugifyDisplayName(in); got != want {
			t.Fatalf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseModelsJSONSlugs(t *testing.T) {
	body := []byte(`{"chat":[
		{"key":"qmodel_38max","display_name":"Qwen3.8-Max","enable":true,"max_input_tokens":200000},
		{"key":"gmodel","display_name":"GLM-5.3","enable":true},
		{"key":"gmodel","display_name":"GLM-5.3","enable":true},
		{"key":"zzmodel","display_name":"神秘模型","enable":true},
		{"key":"offmodel","display_name":"Disabled","enable":false}
	],"assistant":[
		{"key":"assistantonly","display_name":"Assistant-Only","enable":true}
	]}`)
	models, slugToKey, err := parseModelsJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	// chat(4) + assistant-only(1) = 5; enable=false is registered too.
	if len(models) != 5 {
		t.Fatalf("expected 5 models, got %d: %+v", len(models), models)
	}
	if models[0].ID != "qwen3.8-max" || models[0].Name != "Qwen3.8-Max" {
		t.Fatalf("first model: %+v", models[0])
	}
	if models[1].ID != "glm-5.3" {
		t.Fatalf("second model: %+v", models[1])
	}
	// CJK display name falls back to the raw key.
	if models[2].ID != "zzmodel" {
		t.Fatalf("CJK fallback: %+v", models[2])
	}
	// The disabled entry is still registered (upstream decides per request);
	// its ID comes from the slug of display_name "Disabled".
	if models[3].ID != "disabled" {
		t.Fatalf("disabled entry: %+v", models[3])
	}
	// A model that only appears in another scene surfaces too.
	if models[4].ID != "assistant-only" {
		t.Fatalf("scene merge: %+v", models[4])
	}
	if slugToKey["qwen3.8-max"] != "qmodel_38max" {
		t.Fatalf("slug map: %v", slugToKey)
	}
	if slugToKey["glm-5.3"] != "gmodel" {
		t.Fatalf("slug map: %v", slugToKey)
	}
	if slugToKey["gmodel"] != "gmodel" {
		t.Fatalf("raw key entry missing: %v", slugToKey)
	}
}

func TestResolveUpstreamModelKeyPriority(t *testing.T) {
	// Seed the cache for CN with one dynamic entry.
	storeDynamicModels(RegionCN, qdStaticModels()[:1], map[string]string{
		"qwen3.8-max-preview": "qmodel_preview",
		"qmodel_preview":      "qmodel_preview",
	})
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.models = map[string]dynamicModelsEntry{}
		dynamicModelsCache.Unlock()
	}()

	// 1. dynamic slug map wins.
	if got := resolveUpstreamModelKey("qwen3.8-max-preview", RegionCN); got != "qmodel_preview" {
		t.Fatalf("dynamic slug resolution: got %q", got)
	}
	// 2. raw key inside the map passes through.
	if got := resolveUpstreamModelKey("qmodel_preview", RegionCN); got != "qmodel_preview" {
		t.Fatalf("raw key: got %q", got)
	}
	// 3. legacy translation table (deepseek-v4-pro → dmodel).
	if got := resolveUpstreamModelKey("deepseek-v4-pro", RegionGlobal); got != "dmodel" {
		t.Fatalf("legacy table: got %q", got)
	}
	// 4. old internal key passthrough.
	if got := resolveUpstreamModelKey("dmodel", RegionGlobal); got != "dmodel" {
		t.Fatalf("old key: got %q", got)
	}
	// 5. unknown model passes through unchanged.
	if got := resolveUpstreamModelKey("some-new-model", RegionGlobal); got != "some-new-model" {
		t.Fatalf("passthrough: got %q", got)
	}
	// 6. provider-prefixed input is stripped.
	if got := resolveUpstreamModelKey("qoder/deepseek-v4-pro", RegionGlobal); got != "dmodel" {
		t.Fatalf("prefix strip: got %q", got)
	}
	// 7. empty stays empty (callers apply their default).
	if got := resolveUpstreamModelKey("", RegionGlobal); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}

func TestStaticModelIDsAreSlugs(t *testing.T) {
	for _, m := range qdStaticModels() {
		if m.ID != slugifyDisplayName(m.Name) && m.ID != "auto" && m.ID != "lite" {
			t.Fatalf("static ID %q is not the slug of %q", m.ID, m.Name)
		}
		// Every slug (except auto/lite) must translate to its upstream key via
		// the legacy table — this is what makes static-fallback IDs work when
		// no dynamic catalog is available.
		if m.ID == "auto" || m.ID == "lite" {
			continue
		}
		if got := cpaToUpstreamKey(m.ID); got == m.ID {
			t.Fatalf("static ID %q does not translate via cpaToUpstreamKey", m.ID)
		}
	}
}
