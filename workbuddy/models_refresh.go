// models_refresh.go keeps the upstream model catalog warm without waiting for
// the host to ask. CPA re-queries plugin models only on startup, config
// reloads, and auth-file changes (service.go: syncPluginModelRuntime /
// handleAuthUpdates), and /v1/models reads a static in-memory registry — so a
// lazily-fetched catalog goes stale the moment upstream adds a model.
//
// The refresher polls the upstream model list per realm on a timer and, when
// the model ID set actually changes, rewrites every enabled auth file of that
// realm with a models_synced_at stamp via host.auth.save. The write fires the
// host's auth-dir watcher, which re-runs model registration against the
// plugin's now-fresh cache. There is no plugin→host push RPC (pluginabi has
// no such method); the auth-file touch is the only push channel a c-shared
// plugin has. Zero catalog change ⇒ zero writes.
//
// Unlike qoder, the serving cache here is a single slot while the upstream
// list is per-realm (JWT iss decides the endpoint), so the refresher keeps
// its own per-realm baselines: realm A refreshing cannot make realm B's next
// diff look like a change.
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

var errNoRealmAuth = errors.New("no auth available for realm")

// Realm labels for baselines and log lines; detection is per token (JWT iss).
const (
	realmGlobal = "global"
	realmCN     = "cn"
)

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
					refreshAllRealmModels("timer")
				}
				time.Sleep(time.Duration(snap.minutes) * time.Minute)
			}
		}()
	})
}

// refreshAllRealmModels polls both realms. reason tags the trigger in log
// lines ("timer"). A realm with no usable account is a silent skip.
func refreshAllRealmModels(reason string) {
	for _, realm := range []string{realmGlobal, realmCN} {
		changed, count, added, removed, err := refreshRealmModels(realm)
		if err != nil {
			if !errors.Is(err, errNoRealmAuth) {
				wbLogf("warn", "models refresh (%s, %s): %v", reason, realm, err)
			}
			continue
		}
		if !changed {
			// Per-tick summary: proves the refresher is alive between the
			// (rare) real catalog changes.
			wbLogf("info", "models catalog check (%s, %s): %d models, unchanged", reason, realm, count)
			continue
		}
		wbLogf("info", "models catalog changed (%s, %s): +%s -%s",
			reason, realm, strings.Join(added, ","), strings.Join(removed, ","))
		if modelsRefreshConfigSnapshot().push {
			touchRealmAuths(realm)
		}
	}
}

// refreshRealmModels refetches one realm's catalog with any of its accounts
// and diffs the fresh ID set against the realm's baseline. On success the
// serving cache is refreshed too — same data model.for_auth would answer.
func refreshRealmModels(realm string) (changed bool, count int, added, removed []string, err error) {
	before := refreshBaselineIDs(realm)
	models, err := fetchModelsViaAnyAuth(realm)
	if err != nil {
		return false, 0, nil, nil, err
	}
	storeDynamicModels(models)
	after := modelIDSet(models)
	rememberRealmBaseline(realm, after)
	added, removed = diffStringSets(before, after)
	return len(added) > 0 || len(removed) > 0, len(models), added, removed, nil
}

// realmBaselines holds the last fetched ID set per realm, keyed by realm.
var realmBaselines struct {
	sync.Mutex
	ids map[string][]string
}

// refreshBaselineIDs is the ID set the host's registry is assumed to hold for
// a realm: this refresher's last fetch for that realm, else the serving cache
// (warmed by the startup model.for_auth queries), else the static fallback
// that model.static registers. So the first poll after a restart only reports
// a change when the fresh catalog really differs.
func refreshBaselineIDs(realm string) []string {
	realmBaselines.Lock()
	ids := realmBaselines.ids[realm]
	realmBaselines.Unlock()
	if len(ids) > 0 {
		return ids
	}
	if models, ok := lastGoodOrStatic(); ok {
		return modelIDSet(models)
	}
	return modelIDSet(wbModels())
}

func rememberRealmBaseline(realm string, ids []string) {
	realmBaselines.Lock()
	if realmBaselines.ids == nil {
		realmBaselines.ids = map[string][]string{}
	}
	realmBaselines.ids[realm] = ids
	realmBaselines.Unlock()
}

// lastGoodOrStatic returns the cached catalog regardless of age. The bool is
// false when the cache has never been filled — callers fall back to the
// static list. Answering with last-good data matters: model.for_auth
// responses are written into the host's model registry verbatim, so answering
// a transient upstream outage with the static list would silently delete
// every newer model from /v1/models until the next successful query.
func lastGoodOrStatic() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

// fetchModelsViaAnyAuth walks our auth files and asks the upstream models API
// with the first enabled credential of the requested realm. Returns the last
// error when no account can answer (errNoRealmAuth when none even matched).
func fetchModelsViaAnyAuth(realm string) ([]pluginapi.ModelInfo, error) {
	files, err := hostAuthList()
	if err != nil || len(files) == 0 {
		return nil, errNoRealmAuth
	}
	var lastErr error = errNoRealmAuth
	for _, f := range files {
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		if parseDisabledFromAuthJSON(phys.JSON) {
			continue
		}
		// Defense in depth: never read a foreign-domain credential (e.g. a
		// qoder file the host mislabeled workbuddy) — shared {auth,account}
		// shape means parseStored alone cannot tell them apart; domain can.
		if d := strings.ToLower(strings.TrimSpace(domainFromJSON(phys.JSON))); d != "" && !isWorkbuddyDomain(d) {
			continue
		}
		tok := tokenFromAuthJSON(phys.JSON)
		if tok == "" || realmOfToken(tok) != realm {
			continue
		}
		dyn, err := callModelsAPI(tok)
		if err == nil && len(dyn) > 0 {
			return dyn, nil
		}
		if err != nil {
			lastErr = err
		}
		// One attempt per account — a dead token falls through to the next
		// candidate or the baseline.
	}
	return nil, lastErr
}

// tokenFromAuthJSON reads the access token from a nested (plugin OAuth) or
// flat (CPA-Manager-Plus UI) auth file.
func tokenFromAuthJSON(raw []byte) string {
	if tok, ok := extractAccessToken(raw); ok {
		return strings.TrimSpace(tok)
	}
	return ""
}

// realmOfToken classifies a token into its realm. Unparseable tokens are
// treated as CN (the default realm; isGlobalToken already works this way).
func realmOfToken(token string) string {
	if isGlobalToken(token) {
		return realmGlobal
	}
	return realmCN
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

// touchRealmAuths rewrites every enabled workbuddy auth file of one realm
// with a fresh models_synced_at stamp. The content change fires the host's
// auth-dir watcher, which re-runs model registration (per-auth model.for_auth
// plus a full model.static pass) against the plugin's now-fresh cache. The
// raw JSON is round-tripped through map[string]json.RawMessage so no field —
// known or user-added — is lost.
func touchRealmAuths(realm string) {
	files, err := hostAuthList()
	if err != nil || len(files) == 0 {
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	for _, f := range files {
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
		if d := strings.ToLower(strings.TrimSpace(domainFromJSON(phys.JSON))); d != "" && !isWorkbuddyDomain(d) {
			continue
		}
		tok := tokenFromAuthJSON(phys.JSON)
		if tok == "" || realmOfToken(tok) != realm {
			continue
		}
		updated, err := stampAuthJSON(phys.JSON, stamp)
		if err != nil {
			wbLogf("warn", "models push: stamp %s: %v", phys.Name, err)
			continue
		}
		if err := hostAuthSaveJSON(phys.Name, updated); err != nil {
			wbLogf("warn", "models push: save %s: %v", phys.Name, err)
			continue
		}
		wbLogf("info", "models push: touched %s (realm=%s) to re-register models", phys.Name, realm)
	}
}

// stampAuthJSON sets auth.models_synced_at inside the NESTED credential
// object — deliberately not a top-level key. The host's watcher only
// re-registers models when the PARSED auth changes (authEqual on
// coreauth.Auth): a top-level unknown key is dropped by the plugin's auth
// parser, so StorageJSON stays byte-identical and the update is discarded.
// A real storedTokens field flows into AuthData.StorageJSON, so the diff
// fires.
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

// wbLogf sends a line to the host log via the host.log callback. Best-effort:
// an unavailable host API or a logging failure is swallowed — observability
// must never break the path it observes.
func wbLogf(level, format string, args ...any) {
	body, err := json.Marshal(map[string]any{
		"level":   level,
		"message": fmt.Sprintf(format, args...),
	})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}
