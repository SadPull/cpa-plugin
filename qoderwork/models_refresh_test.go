package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = map[string]dynamicModelsEntry{}
	dynamicModelsCache.Unlock()
}

func seedStaleCatalog(region string) {
	storeDynamicModels(region, []pluginapi.ModelInfo{{ID: "stale-model", Name: "Stale"}}, map[string]string{
		"stale-model": "stalekey",
	})
	dynamicModelsCache.Lock()
	e := dynamicModelsCache.models[normalizeRegion(region, defaultRegion())]
	e.fetched = time.Now().Add(-time.Hour) // far beyond dynamicModelsCacheTTL
	dynamicModelsCache.models[normalizeRegion(region, defaultRegion())] = e
	dynamicModelsCache.Unlock()
}

// Serving and resolution must use last-good data regardless of age; only the
// *refetch* decision is TTL-gated.
func TestDynamicSlugKeyServesLastGood(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	seedStaleCatalog(RegionCN)

	if _, ok := cachedDynamicModels(RegionCN); ok {
		t.Fatal("cachedDynamicModels must stay TTL-gated (refetch decision)")
	}
	if key, ok := dynamicSlugKey(RegionCN, "stale-model"); !ok || key != "stalekey" {
		t.Fatalf("dynamicSlugKey should serve last-good: key=%q ok=%v", key, ok)
	}
	if key, ok := dynamicSlugKeyAny("stale-model"); !ok || key != "stalekey" {
		t.Fatalf("dynamicSlugKeyAny should serve last-good: key=%q ok=%v", key, ok)
	}
	if _, ok := lastGoodDynamicModels(RegionCN); !ok {
		t.Fatal("lastGoodDynamicModels should return the stale list")
	}
}

// A failed refetch must answer with the last-good list, never shrink the
// registry to the static fallback.
func TestFetchDynamicModelsFallsBackToLastGood(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	seedStaleCatalog(RegionGlobal)

	// hostAPI is nil in tests, so the refetch path fails and the stale
	// catalog must win over qdStaticModels().
	models := fetchDynamicModels(RegionGlobal)
	if len(models) != 1 || models[0].ID != "stale-model" {
		t.Fatalf("expected last-good catalog, got %+v", models)
	}
}

func TestFetchDynamicModelsStaticFallbackWithoutCache(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	models := fetchDynamicModels(RegionCN)
	if len(models) != len(qdStaticModels()) {
		t.Fatalf("expected static fallback, got %d models", len(models))
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

// Baseline = cached catalog when warm, else the static list the host's
// registry actually holds. The first poll after a restart therefore only
// reports a change when the fresh catalog really differs from static.
func TestRefreshBaselineIDs(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	if got := refreshBaselineIDs(RegionCN); !reflect.DeepEqual(got, modelIDSet(qdStaticModels())) {
		t.Fatalf("cold baseline should be the static IDs, got %v", got)
	}
	seedStaleCatalog(RegionCN)
	if got := refreshBaselineIDs(RegionCN); !reflect.DeepEqual(got, []string{"stale-model"}) {
		t.Fatalf("warm baseline should be the cached IDs, got %v", got)
	}
}

// With no host API the walk cannot find any account: silent skip, no change.
func TestRefreshRegionModelsNoAuth(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()

	changed, count, added, removed, err := refreshRegionModels(RegionCN)
	if changed || count != 0 || added != nil || removed != nil {
		t.Fatalf("unexpected change report: %v %d %+v %+v", changed, count, added, removed)
	}
	if err != errNoRegionAuth {
		t.Fatalf("err = %v, want errNoRegionAuth", err)
	}
}

func TestStampAuthJSON(t *testing.T) {
	raw := []byte(`{"type":"qoder","custom_field":{"nested":1},"auth":{"accessToken":"dt-x"}}`)
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
	// The stamp must land INSIDE the nested auth object as a real
	// storedTokens field — a top-level key never reaches the host's parsed
	// auth, so its diff (authEqual) would discard the update unregistered.
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(m["auth"], &auth); err != nil {
		t.Fatal(err)
	}
	if string(auth["accessToken"]) != `"dt-x"` {
		t.Fatalf("nested auth mutated: %s", m["auth"])
	}
	var stamp string
	if err := json.Unmarshal(auth["models_synced_at"], &stamp); err != nil || stamp != "2026-09-06T00:00:00Z" {
		t.Fatalf("auth.models_synced_at = %s (err %v)", auth["models_synced_at"], err)
	}
	if _, err := stampAuthJSON([]byte(`[1,2]`), "x"); err == nil {
		t.Fatal("non-object auth JSON must be rejected")
	}
	if _, err := stampAuthJSON([]byte(`{"type":"qoder"}`), "x"); err == nil {
		t.Fatal("missing nested auth object must be rejected")
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

func TestKickModelsRefreshThrottles(t *testing.T) {
	defer setModelsRefreshConfig(true, 10, true)
	setModelsRefreshConfig(true, 10, true)

	modelsRefreshKick.Lock()
	modelsRefreshKick.last = time.Now().Add(-time.Hour)
	modelsRefreshKick.Unlock()

	kickModelsRefresh() // fires: spawns the goroutine, stamps last
	modelsRefreshKick.Lock()
	first := modelsRefreshKick.last
	modelsRefreshKick.Unlock()
	if time.Since(first) > time.Minute {
		t.Fatal("first kick should stamp the throttle window")
	}

	kickModelsRefresh() // within the 10-minute window: must be a no-op
	modelsRefreshKick.Lock()
	second := modelsRefreshKick.last
	modelsRefreshKick.Unlock()
	if !second.Equal(first) {
		t.Fatal("second kick inside the window must not re-arm")
	}
}
