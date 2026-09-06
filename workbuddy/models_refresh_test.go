package main

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
}

func resetRealmBaselines() {
	realmBaselines.Lock()
	realmBaselines.ids = nil
	realmBaselines.Unlock()
}

func seedStaleCatalog() {
	storeDynamicModels([]pluginapi.ModelInfo{{ID: "stale-model", Name: "Stale"}})
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now().Add(-time.Hour) // far beyond dynamicModelsCacheTTL
	dynamicModelsCache.Unlock()
}

// The serving cache is last-good regardless of age; only the *refetch*
// decision is TTL-gated.
func TestLastGoodOrStatic(t *testing.T) {
	resetDynamicModelsCache()
	resetRealmBaselines()
	defer func() { resetDynamicModelsCache(); resetRealmBaselines() }()

	if models, ok := lastGoodOrStatic(); ok {
		t.Fatalf("empty cache must report false, got %+v", models)
	}
	if models := fetchDynamicModelsFromStorage(nil); len(models) != len(wbModels()) {
		t.Fatalf("no cache and no token: expected static fallback, got %d models", len(models))
	}

	seedStaleCatalog()
	if _, ok := cachedDynamicModels(); ok {
		t.Fatal("cachedDynamicModels must stay TTL-gated (refetch decision)")
	}
	models, ok := lastGoodOrStatic()
	if !ok || len(models) != 1 || models[0].ID != "stale-model" {
		t.Fatalf("lastGoodOrStatic should return the stale list: %+v ok=%v", models, ok)
	}
	// Empty storage means no token → no upstream call (hermetic) → last-good
	// wins over the static fallback.
	got := fetchDynamicModelsFromStorage(nil)
	if len(got) != 1 || got[0].ID != "stale-model" {
		t.Fatalf("fetchDynamicModelsFromStorage should serve last-good, got %+v", got)
	}
}

// With no host API the walk cannot find any account: silent skip, no change.
func TestRefreshRealmModelsNoAuth(t *testing.T) {
	resetDynamicModelsCache()
	resetRealmBaselines()
	defer func() { resetDynamicModelsCache(); resetRealmBaselines() }()

	changed, added, removed, err := refreshRealmModels(realmCN)
	if changed || added != nil || removed != nil {
		t.Fatalf("unexpected change report: %v %+v %+v", changed, added, removed)
	}
	if err != errNoRealmAuth {
		t.Fatalf("err = %v, want errNoRealmAuth", err)
	}
}

// Baseline precedence: refresher's per-realm baseline → serving cache →
// static list.
func TestRefreshBaselineIDs(t *testing.T) {
	resetDynamicModelsCache()
	resetRealmBaselines()
	defer func() { resetDynamicModelsCache(); resetRealmBaselines() }()

	if got := refreshBaselineIDs(realmCN); !reflect.DeepEqual(got, modelIDSet(wbModels())) {
		t.Fatalf("cold baseline should be the static IDs, got %v", got)
	}
	seedStaleCatalog()
	if got := refreshBaselineIDs(realmCN); !reflect.DeepEqual(got, []string{"stale-model"}) {
		t.Fatalf("warm serving cache should back the baseline, got %v", got)
	}
	rememberRealmBaseline(realmCN, []string{"cn-model"})
	if got := refreshBaselineIDs(realmCN); !reflect.DeepEqual(got, []string{"cn-model"}) {
		t.Fatalf("realm baseline should win, got %v", got)
	}
	// The other realm keeps its own baseline — a CN refresh cannot make a
	// global diff look like a change.
	if got := refreshBaselineIDs(realmGlobal); !reflect.DeepEqual(got, []string{"stale-model"}) {
		t.Fatalf("per-realm isolation broken, got %v", got)
	}
}

func TestDiffStringSets(t *testing.T) {
	added, removed := diffStringSets(
		[]string{"a", "b", "c"},
		[]string{"b", "c", "d", "d"}, // duplicates must not inflate "added"
	)
	if !reflect.DeepEqual(added, []string{"d"}) {
		t.Fatalf("added = %v", added)
	}
	if !reflect.DeepEqual(removed, []string{"a"}) {
		t.Fatalf("removed = %v", removed)
	}
}

func TestRealmOfToken(t *testing.T) {
	jwtWithIss := func(iss string) string {
		payload, err := json.Marshal(map[string]string{"iss": iss})
		if err != nil {
			t.Fatal(err)
		}
		return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	}
	if got := realmOfToken(jwtWithIss("https://auth.workbuddy.ai/realms/wb")); got != realmGlobal {
		t.Fatalf("global token classified as %q", got)
	}
	if got := realmOfToken(jwtWithIss("https://sso.codebuddy.cn/realms/cb")); got != realmCN {
		t.Fatalf("CN token classified as %q", got)
	}
	if got := realmOfToken("not-a-jwt"); got != realmCN {
		t.Fatalf("garbage token classified as %q, want CN default", got)
	}
}

func TestStampAuthJSON(t *testing.T) {
	raw := []byte(`{"type":"workbuddy","custom_field":{"nested":1},"auth":{"accessToken":"wb-x"}}`)
	out, err := stampAuthJSON(raw, "2026-09-06T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["custom_field"]; !ok {
		t.Fatal("unknown fields must survive the round-trip")
	}
	if string(m["auth"]) != `{"accessToken":"wb-x"}` {
		t.Fatalf("nested auth mutated: %s", m["auth"])
	}
	var stamp string
	if err := json.Unmarshal(m["models_synced_at"], &stamp); err != nil || stamp != "2026-09-06T00:00:00Z" {
		t.Fatalf("models_synced_at = %s (err %v)", m["models_synced_at"], err)
	}
	if _, err := stampAuthJSON([]byte(`[1,2]`), "x"); err == nil {
		t.Fatal("non-object auth JSON must be rejected")
	}
}

func TestSetModelsRefreshConfigClamps(t *testing.T) {
	defer setModelsRefreshConfig(true, 10, true)

	setModelsRefreshConfig(false, 0, false)
	snap := modelsRefreshConfigSnapshot()
	if snap.enabled || snap.push || snap.minutes != 1 {
		t.Fatalf("snapshot = %+v, want {false 1 false}", snap)
	}
}

func TestConfigureModelsRefreshKeys(t *testing.T) {
	defer setModelsRefreshConfig(true, 10, true)

	// config_yaml is a []byte on the wire, i.e. base64 in JSON — marshal it
	// through the same struct the host uses, not a hand-built string.
	configureWith := func(yaml string) {
		b, err := json.Marshal(struct {
			ConfigYAML []byte `json:"config_yaml"`
		}{ConfigYAML: []byte(yaml)})
		if err != nil {
			t.Fatal(err)
		}
		configure(b)
	}

	// Keys absent → defaults (reconfigure resets like the other knobs).
	configureWith("checkin_auto: true\n")
	if snap := modelsRefreshConfigSnapshot(); snap != (modelsRefreshConfig{enabled: true, minutes: 10, push: true}) {
		t.Fatalf("defaults = %+v", snap)
	}

	configureWith("\nmodels_refresh: false\nmodels_refresh_minutes: 3\nmodels_refresh_push: no\n")
	snap := modelsRefreshConfigSnapshot()
	if snap.enabled || snap.minutes != 3 || snap.push {
		t.Fatalf("parsed = %+v", snap)
	}

	configureWith("models_refresh_minutes: 0\n")
	if snap := modelsRefreshConfigSnapshot(); snap.minutes != 1 {
		t.Fatalf("clamp = %+v", snap)
	}
}
