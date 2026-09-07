// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// qdStaticModels is the static fallback model list. IDs are slugs of the
// upstream display names (execution resolves them via cpaToUpstreamKey);
// "auto"/"lite" are the CLI defaults. Mirrors the live catalog snapshot
// (2026-09-06): dynamic discovery replaces this when an account can answer.
func qdStaticModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "auto", Name: "Auto", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "lite", Name: "Lite", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.8-max", Name: "Qwen3.8-Max", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.7-max", Name: "Qwen3.7-Max", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.7-plus", Name: "Qwen3.7-Plus", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.7-flash", Name: "Qwen3.7-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.3", Name: "GLM-5.3", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.7-code", Name: "Kimi-K2.7-Code", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "minimax-m2.7", Name: "MiniMax-M2.7", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

func cachedDynamicModels(region string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.models[normalizeRegion(region, defaultRegion())]
	if !ok {
		return nil, false
	}
	if len(entry.list) > 0 && time.Since(entry.fetched) < dynamicModelsCacheTTL {
		return entry.list, true
	}
	return nil, false
}

func storeDynamicModels(region string, models []pluginapi.ModelInfo, slugToKey map[string]string) {
	region = normalizeRegion(region, defaultRegion())
	dynamicModelsCache.Lock()
	dynamicModelsCache.models[region] = dynamicModelsEntry{list: models, slugToKey: slugToKey, fetched: time.Now()}
	dynamicModelsCache.Unlock()
}

// dynamicSlugKey resolves a client-facing model ID against the dynamic cache
// of one region (slug(display_name) or raw upstream key → upstream key).
//
// No age check: the cache is served last-good regardless of staleness. The
// 5-minute TTL only gates *refetch* decisions (cachedDynamicModels); serving
// real upstream data from an hour ago beats falling back to the legacy table
// and losing every model released since. The background refresher
// (models_refresh.go) keeps the cache current.
func dynamicSlugKey(region, model string) (string, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.models[normalizeRegion(region, defaultRegion())]
	if !ok {
		return "", false
	}
	key, ok := entry.slugToKey[strings.ToLower(strings.TrimSpace(model))]
	return key, ok
}

// dynamicSlugKeyAny resolves against every cached region's map — for clients
// that reference a model by its slug regardless of which account serves it.
// Last-good semantics, same as dynamicSlugKey.
func dynamicSlugKeyAny(model string) (string, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	for _, entry := range dynamicModelsCache.models {
		if key, ok := entry.slugToKey[strings.ToLower(strings.TrimSpace(model))]; ok {
			return key, true
		}
	}
	return "", false
}

// lastGoodDynamicModels returns the cached list of one region regardless of
// age. Served when a refetch fails so a transient upstream outage never
// shrinks the host's model registry down to the static fallback.
func lastGoodDynamicModels(region string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	entry, ok := dynamicModelsCache.models[normalizeRegion(region, defaultRegion())]
	if !ok || len(entry.list) == 0 {
		return nil, false
	}
	return entry.list, true
}

// resolveUpstreamModelKey maps a client-facing model name onto the upstream
// key the realm's inference endpoint understands. Resolution order:
//  1. dynamic catalog map (slug of display_name, or the raw key itself)
//  2. legacy translation table (friendly names + old internal keys)
//  3. passthrough (unknown models go to the upstream as-is; the server
//     silently routes unknown keys to auto)
//
// Empty input returns "" — callers apply their own realm default.
func resolveUpstreamModelKey(model, region string) string {
	m := strings.TrimSpace(stripProviderPrefix(model))
	if m == "" {
		return ""
	}
	if key, ok := dynamicSlugKey(region, m); ok {
		return key
	}
	if key, ok := dynamicSlugKeyAny(m); ok {
		return key
	}
	if key := cpaToUpstreamKey(m); key != m {
		return key
	}
	// Total miss — a model the catalog never knew. Schedule one throttled
	// background refresh so the mapping appears for the next request; this
	// request still resolves via the passthrough below.
	kickModelsRefresh()
	return m
}

// fetchDynamicModels discovers models with accounts of the given region.
// Fallback chain: fresh cache → refetch via any account → last-good cache →
// static list. The last-good step matters: model.for_auth answers get written
// into the host's model registry verbatim, so answering a transient upstream
// outage with the static list would silently delete every newer model from
// /v1/models until the next successful query.
func fetchDynamicModels(region string) []pluginapi.ModelInfo {
	region = normalizeRegion(region, defaultRegion())
	if models, ok := cachedDynamicModels(region); ok {
		return models
	}
	if dyn, err := fetchModelsViaAnyAuth(region); err == nil {
		return dyn
	}
	if models, ok := lastGoodDynamicModels(region); ok {
		return models
	}
	return qdStaticModels()
}

// fetchModelsViaAnyAuth walks our auth files and asks the upstream models API
// with the first credential matching the requested region. Returns the last
// error when no account can answer (errNoRegionAuth when none even matched).
func fetchModelsViaAnyAuth(region string) ([]pluginapi.ModelInfo, error) {
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		return nil, errNoRegionAuth
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the qoder models API with a workbuddy token.)
	prefix := providerName + "-"
	var lastErr error = errNoRegionAuth
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			continue
		}
		// Defense in depth: never send a foreign-domain credential (e.g. a
		// workbuddy file the host mislabeled qoder) to the Qoder models
		// API. Both plugins share the nested {auth,account} shape, so
		// parseStored alone cannot tell them apart — domain can.
		if d := strings.ToLower(strings.TrimSpace(sa.Auth.Domain)); d != "" && !isQoderDomain(d) {
			continue
		}
		if regionForAuth(sa) != region {
			continue
		}
		dyn, err := callModelsAPI(sa)
		if err == nil && len(dyn) > 0 {
			return dyn, nil
		}
		if err != nil {
			lastErr = err
		}
		// One attempt per account — a dead token falls through to the next
		// candidate or the static list.
	}
	return nil, lastErr
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	sa, err := parseStored(storageJSON)
	region := defaultRegion()
	if err == nil && sa != nil {
		region = regionForAuth(sa)
	}
	if models, ok := cachedDynamicModels(region); ok {
		return models
	}
	if err == nil && sa != nil {
		if dyn, err2 := callModelsAPI(sa); err2 == nil && len(dyn) > 0 {
			return dyn // callModelsAPI stores the cache (with the slug map)
		}
	}
	return fetchDynamicModels(region)
}

// callModelsAPI GETs the model list for one account's realm.
//   - CN: gateway /algo/api/v2/model/list?Encode=1 with full COSY signing
//     (GET still signs the encoded empty body — same as inference).
//   - global: Bearer GET against the api2-v2 model list (speculative path —
//     the caller falls back to the static list on any error).
//
// Returns plain JSON (not QoderEncoding).
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	region := regionForAuth(sa)
	spec := specFor(region)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.ModelsURL, nil)
	if err != nil {
		return nil, err
	}
	if region == RegionCN {
		// The gateway validates the COSY signature against the body it
		// actually receives. For a GET with no body that means signing the
		// EMPTY string — signing a placeholder body (e.g. qoderEncode("{}"))
		// while transmitting nothing yields 403 "Signature invalid"
		// (verified live: signed-empty returns the full 13-model catalog).
		if err := applyCosyHeaders(req, sa, "", spec.ModelsURL, "", false); err != nil {
			return nil, fmt.Errorf("cosy sign: %w", err)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	models, slugToKey, err := parseModelsJSON(resp.Body)
	if err != nil {
		return nil, err
	}
	storeDynamicModels(region, models, slugToKey)
	return models, nil
}

// slugifyDisplayName derives a client-facing model ID from the upstream
// display name: lowercase, whitespace/underscore → "-", keep [a-z0-9.-],
// trim stray dashes. "Qwen3.8-Max-Preview" → "qwen3.8-max-preview",
// "GLM-5.2" → "glm-5.2". Returns "" when nothing survives (e.g. a purely
// CJK display name) — callers fall back to the raw upstream key.
func slugifyDisplayName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := true // avoid leading dash
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
			lastDash = false
		case r == '-', r == '_', r == ' ', r == '\t':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// skip characters that would make an awkward model ID
		}
	}
	return strings.Trim(b.String(), "-")
}

// parseModelsJSON decodes the shared model-list shape
// {"chat":[{key,display_name,enable,max_input_tokens,...}], "developer":[...], ...}
// and returns the model list plus a slug(display_name) → upstream key map for
// execution-time resolution.
//
// Scenes are MERGED (dedup by key, chat first) so models that only appear in
// another scene still surface; enable=false entries are registered too — the
// upstream routing layer decides per request, and a missing model is worse
// than a possibly-rejected one.
func parseModelsJSON(body []byte) ([]pluginapi.ModelInfo, map[string]string, error) {
	// Response is plain JSON: {"chat":[{key,display_name,...}], "developer":[...], ...}
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, nil, fmt.Errorf("models parse: %w", err)
	}
	// Parse the preferred scene first, then every other scene in stable order.
	sceneOrder := []string{"chat", "app", "assistant", "developer", "inline", "quest", "qwork", "qwake", "experts"}
	sceneOrder = append(sceneOrder, sortedSceneKeys(apiResp, sceneOrder)...)
	var out []pluginapi.ModelInfo
	slugToKey := map[string]string{}
	seenKeys := map[string]string{} // upstream key -> first scene
	usedIDs := map[string]string{}  // model ID -> upstream key (dedup)
	for _, scene := range sceneOrder {
		rawScene, ok := apiResp[scene]
		if !ok {
			continue
		}
		var models []struct {
			Key            string  `json:"key"`
			DisplayName    string  `json:"display_name"`
			Enable         bool    `json:"enable"`
			IsReasoning    bool    `json:"is_reasoning"`
			IsVL           bool    `json:"is_vl"`
			MaxInputTokens int64   `json:"max_input_tokens"`
			PriceFactor    float64 `json:"price_factor"`
		}
		if err := json.Unmarshal(rawScene, &models); err != nil {
			continue // non-list payload for this scene — skip
		}
		for _, m := range models {
			key := strings.ToLower(strings.TrimSpace(m.Key))
			if key == "" {
				continue
			}
			if _, dup := seenKeys[key]; dup {
				continue // already registered from a preferred scene
			}
			seenKeys[key] = scene
			ctx2 := int64(180000)
			if m.MaxInputTokens > 0 {
				ctx2 = m.MaxInputTokens
			}
			id := slugifyDisplayName(m.DisplayName)
			if id == "" {
				id = key
			}
			// Dedup: exact duplicates (same slug, same key) are skipped; two
			// different keys slugging to the same ID get a key suffix.
			if prev, dup := usedIDs[id]; dup {
				if prev == m.Key {
					continue
				}
				id = id + "-" + key
			}
			usedIDs[id] = m.Key
			slugToKey[strings.ToLower(id)] = m.Key
			// The raw upstream key keeps resolving too (back-compat).
			if _, exists := slugToKey[key]; !exists {
				slugToKey[key] = m.Key
			}
			out = append(out, pluginapi.ModelInfo{
				ID:                         id,
				Name:                       m.DisplayName,
				ContextLength:              ctx2,
				MaxCompletionTokens:        8192,
				OwnedBy:                    providerName,
				SupportedGenerationMethods: []string{"chat"},
			})
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no chat models in catalog")
	}
	return out, slugToKey, nil
}

// sortedSceneKeys returns the remaining response keys (not in the preferred
// order) in sorted order, so scene merging is deterministic across calls.
func sortedSceneKeys(apiResp map[string]json.RawMessage, known []string) []string {
	seen := map[string]bool{}
	for _, k := range known {
		seen[k] = true
	}
	var rest []string
	for k := range apiResp {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	return rest
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the qoder provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := fetchDynamicModels(defaultRegion())
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
