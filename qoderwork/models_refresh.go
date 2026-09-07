// models_refresh.go keeps the upstream model catalog warm without waiting for
// the host to ask. CPA re-queries plugin models only on startup, config
// reloads, and auth-file changes (v7.2.30 service.go: syncPluginModelRuntime /
// handleAuthUpdates), and /v1/models reads a static in-memory registry — so a
// lazily-fetched catalog goes stale the moment upstream adds a model, and the
// chat path loses the slug→key mapping for it.
//
// The refresher polls the upstream model list per realm on a timer and, when
// the model ID set actually changes, rewrites every enabled auth file of that
// realm with a models_synced_at stamp via host.auth.save. The write fires the
// host's auth-dir watcher, which re-runs model registration against the
// plugin's now-fresh cache. There is no plugin→host push RPC (pluginabi has
// no such method); the auth-file touch is the only push channel a c-shared
// plugin has. Zero catalog change ⇒ zero writes.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// errNoRegionAuth reports that no credential matched the requested realm —
// the refresher treats it as a silent skip, not a failure.
var errNoRegionAuth = errors.New("no auth available for region")

// modelsRefreshConfig is the plugin-config snapshot for the catalog refresher.
type modelsRefreshConfig struct {
	enabled bool // models_refresh
	minutes int  // models_refresh_minutes, minimum 1
	push    bool // models_refresh_push
}

var (
	modelsRefreshCfg   = modelsRefreshConfig{enabled: true, minutes: 10, push: true}
	modelsRefreshCfgMu sync.RWMutex
)

func modelsRefreshConfigSnapshot() modelsRefreshConfig {
	modelsRefreshCfgMu.RLock()
	defer modelsRefreshCfgMu.RUnlock()
	return modelsRefreshCfg
}

// setModelsRefreshConfig applies a reconfigure, clamping the interval. The
// new interval takes effect from the next tick — the loop sleeps between
// rounds rather than holding an adjustable timer.
func setModelsRefreshConfig(enabled bool, minutes int, push bool) {
	if minutes < 1 {
		minutes = 1
	}
	modelsRefreshCfgMu.Lock()
	modelsRefreshCfg = modelsRefreshConfig{enabled: enabled, minutes: minutes, push: push}
	modelsRefreshCfgMu.Unlock()
}

// modelsRefresherFirstDelay lets the host finish its startup model queries
// (which warm the cache themselves) before the first poll.
const modelsRefresherFirstDelay = time.Minute

var modelsRefresherOnce sync.Once

// startModelsRefresher launches the background refresher once per process;
// later reconfigures only swap the config snapshot.
func startModelsRefresher() {
	modelsRefresherOnce.Do(func() {
		go func() {
			time.Sleep(modelsRefresherFirstDelay)
			for {
				snap := modelsRefreshConfigSnapshot()
				if snap.enabled {
					refreshAllRegionModels("timer")
				}
				time.Sleep(time.Duration(snap.minutes) * time.Minute)
			}
		}()
	})
}

// kickModelsRefresh schedules one throttled refresh when execution hits a
// model the catalog doesn't know — the demand-driven backstop for a catalog
// that went stale between ticks (or with models_refresh disabled). The kick
// is deliberately allowed even when the timer is off: it only fires when a
// client actually asks for an unknown model.
var modelsRefreshKick struct {
	sync.Mutex
	last time.Time
}

func kickModelsRefresh() {
	modelsRefreshKick.Lock()
	defer modelsRefreshKick.Unlock()
	snap := modelsRefreshConfigSnapshot()
	interval := time.Duration(snap.minutes) * time.Minute
	if !snap.enabled {
		// Timer off: still throttle, using the refetch TTL as the window.
		interval = dynamicModelsCacheTTL
	}
	if time.Since(modelsRefreshKick.last) < interval {
		return
	}
	modelsRefreshKick.last = time.Now()
	go refreshAllRegionModels("kick")
}

// refreshAllRegionModels polls every realm the plugin may hold accounts for.
// reason tags the trigger in log lines ("timer" / "kick").
func refreshAllRegionModels(reason string) {
	for _, region := range []string{RegionGlobal, RegionCN} {
		changed, count, added, removed, err := refreshRegionModels(region)
		if err != nil {
			if !errors.Is(err, errNoRegionAuth) {
				qdLogf("warn", "models refresh (%s, %s): %v", reason, region, err)
			}
			continue
		}
		if !changed {
			// Per-tick summary: proves the refresher is alive between the
			// (rare) real catalog changes.
			qdLogf("info", "models catalog check (%s, %s): %d models, unchanged", reason, region, count)
			continue
		}
		qdLogf("info", "models catalog changed (%s, %s): +%s -%s",
			reason, region, strings.Join(added, ","), strings.Join(removed, ","))
		if modelsRefreshConfigSnapshot().push {
			touchRegionAuths(region)
		}
	}
}

// refreshRegionModels refetches one realm's catalog with any of its accounts
// and diffs the fresh ID set against the baseline. callModelsAPI stores the
// fresh list (and slug map) on success; a failure leaves the cache — and the
// host registry — untouched.
func refreshRegionModels(region string) (changed bool, count int, added, removed []string, err error) {
	region = normalizeRegion(region, defaultRegion())
	before := refreshBaselineIDs(region)
	models, err := fetchModelsViaAnyAuth(region)
	if err != nil {
		return false, 0, nil, nil, err
	}
	added, removed = diffStringSets(before, modelIDSet(models))
	return len(added) > 0 || len(removed) > 0, len(models), added, removed, nil
}

// refreshBaselineIDs is the ID set the host's registry is assumed to hold for
// a region: the cached catalog when one exists, else the static fallback that
// model.static answered with. A cold cache therefore counts as changed only
// when the fresh list really differs from the static list — the first poll
// after a CPA restart doesn't re-register an unchanged catalog.
func refreshBaselineIDs(region string) []string {
	if ids := dynamicModelIDs(region); len(ids) > 0 {
		return ids
	}
	return modelIDSet(qdStaticModels())
}

// dynamicModelIDs snapshots the cached catalog's model IDs for one region.
func dynamicModelIDs(region string) []string {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.models[normalizeRegion(region, defaultRegion())]
	if !ok {
		return nil
	}
	return modelIDSet(entry.list)
}

func modelIDSet(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// diffStringSets returns the strings present in exactly one of the two sets.
// Duplicate entries in either input are collapsed, and the results keep their
// first-occurrence order so log lines are deterministic.
func diffStringSets(before, after []string) (added, removed []string) {
	a := make(map[string]struct{}, len(before))
	for _, s := range before {
		a[s] = struct{}{}
	}
	b := make(map[string]struct{}, len(after))
	for _, s := range after {
		b[s] = struct{}{}
	}
	seen := make(map[string]struct{}, len(after))
	for _, s := range after {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		if _, ok := a[s]; !ok {
			added = append(added, s)
		}
	}
	seen = make(map[string]struct{}, len(before))
	for _, s := range before {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		if _, ok := b[s]; !ok {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// touchRegionAuths rewrites every enabled qoder auth file of one realm with a
// fresh models_synced_at stamp. The content change fires the host's auth-dir
// watcher, which re-runs model registration (per-auth model.for_auth plus a
// full model.static pass) against the plugin's now-fresh cache. The raw JSON
// is round-tripped through map[string]json.RawMessage so no field — known or
// user-added — is lost.
func touchRegionAuths(region string) {
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	prefix := providerName + "-"
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		if strings.TrimSpace(phys.Name) == "" {
			continue
		}
		if parseDisabledFromAuthJSON(phys.JSON) {
			continue // disabled accounts are not registered for models anyway
		}
		// Same ownership + realm guards as fetchModelsViaAnyAuth — never
		// rewrite a credential that is not ours or belongs to another realm.
		if d := strings.ToLower(strings.TrimSpace(domainFromJSON(phys.JSON))); d != "" && !isQoderDomain(d) {
			continue
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil || regionForAuth(sa) != region {
			continue
		}
		updated, err := stampAuthJSON(phys.JSON, stamp)
		if err != nil {
			qdLogf("warn", "models push: stamp %s: %v", phys.Name, err)
			continue
		}
		if err := hostAuthSaveJSON(phys.Name, updated); err != nil {
			qdLogf("warn", "models push: save %s: %v", phys.Name, err)
			continue
		}
		qdLogf("info", "models push: touched %s (region=%s) to re-register models", phys.Name, region)
	}
}

// stampAuthJSON sets auth.models_synced_at inside the NESTED credential
// object — deliberately not a top-level key. The host's watcher only
// re-registers models when the PARSED auth changes (authEqual on
// coreauth.Auth): a top-level unknown key is dropped by the plugin's auth
// parser, so StorageJSON stays byte-identical and the update is discarded
// (observed live: WRITE processed, zero re-registration). A real
// storedTokens field flows into AuthData.StorageJSON, so the diff fires.
func stampAuthJSON(raw []byte, stamp string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("auth json is not an object")
	}
	var auth map[string]json.RawMessage
	if len(m["auth"]) == 0 {
		return nil, errors.New("nested auth object is missing")
	}
	if err := json.Unmarshal(m["auth"], &auth); err != nil || auth == nil {
		return nil, errors.New("nested auth is not an object")
	}
	encoded, err := json.Marshal(stamp)
	if err != nil {
		return nil, err
	}
	auth["models_synced_at"] = encoded
	m["auth"], err = json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// qdLogf sends a line to the host log via the host.log callback. Best-effort:
// an unavailable host API or a logging failure is swallowed — observability
// must never break the path it observes.
func qdLogf(level, format string, args ...any) {
	body, err := json.Marshal(map[string]any{
		"level":   level,
		"message": fmt.Sprintf(format, args...),
	})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}
