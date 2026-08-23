// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels mirrors the upstream "cli" agent model list (order included) as
// served by GET /console/enterprises/personal/models. Snapshot taken
// 2026-08-23 from the CN realm; ContextLength/MaxCompletionTokens come from
// the upstream maxInputTokens/maxOutputTokens fields. This list is only the
// no-auth / dynamic-fetch-failure fallback — logged-in accounts get the live
// list via callModelsAPI.
func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "auto", Name: "Auto", ContextLength: 168000, MaxCompletionTokens: 32000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3", Name: "Hy3", ContextLength: 192000, MaxCompletionTokens: 64000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3-x", Name: "Hy3 X", ContextLength: 192000, MaxCompletionTokens: 64000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.3", Name: "GLM-5.3", ContextLength: 1000000, MaxCompletionTokens: 48000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1000000, MaxCompletionTokens: 48000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.1", Name: "GLM-5.1", ContextLength: 200000, MaxCompletionTokens: 48000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5v-turbo", Name: "GLM-5v-Turbo", ContextLength: 200000, MaxCompletionTokens: 64000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k3-1", Name: "Kimi-K3", ContextLength: 1000000, MaxCompletionTokens: 32000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.7", Name: "Kimi-K2.7-Code", ContextLength: 256000, MaxCompletionTokens: 32000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.6", Name: "Kimi-K2.6", ContextLength: 256000, MaxCompletionTokens: 32000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "minimax-m3", Name: "MiniMax-M3", ContextLength: 512000, MaxCompletionTokens: 128000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash", ContextLength: 1000000, MaxCompletionTokens: 50000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextLength: 1000000, MaxCompletionTokens: 50000, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

func cachedDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsCacheTTL {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

func storeDynamicModels(models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = models
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	accessToken := ""
	if len(storageJSON) > 0 {
		if tok, ok := extractAccessToken(storageJSON); ok {
			accessToken = tok
		}
	}
	if accessToken == "" {
		return wbModels()
	}
	if dyn, err := callModelsAPI(accessToken); err == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		return dyn
	}
	return wbModels()
}

// fetchDynamicModels calls the WorkBuddy API to get the latest model list.
// Falls back to the hardcoded list on any error.
// extractAccessToken handles both flat (CPA UI) and nested (plugin OAuth) auth file shapes.
func extractAccessToken(raw []byte) (string, bool) {
	// flat shape from CPA-Manager-Plus UI
	var flat struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && strings.TrimSpace(flat.AccessToken) != "" {
		return flat.AccessToken, true
	}
	// nested shape from plugin OAuth
	var nested storedAuth
	if err := json.Unmarshal(raw, &nested); err == nil && strings.TrimSpace(nested.Auth.AccessToken) != "" {
		return nested.Auth.AccessToken, true
	}
	return "", false
}

// realmFromToken decodes the JWT iss claim to determine the account realm.
// Global tokens have iss=...workbuddy.ai...; CN tokens have iss=...codebuddy.cn...
// Returns true if the token is Global.
func isGlobalToken(accessToken string) bool {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return false
	}
	payload := parts[1]
	// base64url padding
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var claims struct {
		ISS string `json:"iss"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return strings.Contains(strings.ToLower(claims.ISS), "workbuddy.ai")
}

// upstreamModel is one entry of data.models[] from the models API. The API
// renamed its token fields (contextWindow/maxTokens → maxInputTokens/
// maxOutputTokens); both spellings are accepted so either realm shape works.
type upstreamModel struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Credits            string          `json:"credits"`
	Configurable       bool            `json:"configurable"`
	Configured         bool            `json:"configured"`
	IsDefault          bool            `json:"isDefault"`
	SupportsImages     bool            `json:"supportsImages"`
	SupportsReasoning  bool            `json:"supportsReasoning"`
	OnlyReasoning      bool            `json:"onlyReasoning"`
	Reasoning          json.RawMessage `json:"reasoning"`
	DisabledMultimodal bool            `json:"disabledMultimodal"`
	Disabled           bool            `json:"disabled"`
	DisabledReason     string          `json:"disabledReason"`
	ContextWindow      json.RawMessage `json:"contextWindow"`
	MaxTokens          json.RawMessage `json:"maxTokens"`
	MaxInputTokens     json.RawMessage `json:"maxInputTokens"`
	MaxOutputTokens    json.RawMessage `json:"maxOutputTokens"`
}

// rawNumber decodes the first raw JSON field that holds a number, letting a
// renamed upstream field fall back to its legacy spelling.
func rawNumber(fields ...json.RawMessage) int64 {
	for _, f := range fields {
		if len(f) == 0 {
			continue
		}
		var v float64
		if err := json.Unmarshal(f, &v); err == nil {
			return int64(v)
		}
	}
	return 0
}

// callModelsAPI GETs /console/enterprises/personal/models from the upstream.
// Uses the shared client (connection pooling) with a per-request 15s budget;
// the shared client's own 120s timeout stays as the outer bound.
func callModelsAPI(accessToken string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Model discovery is per-realm: Global tokens must query workbuddy.ai,
	// not copilot.tencent.com (which 500s for Global tokens). Decode JWT iss.
	isGlobal := isGlobalToken(accessToken)
	modelsURL := endpointModels
	origin := originReferer
	if isGlobal {
		modelsURL = upstreamBaseGlobal + "/console/enterprises/personal/models"
		origin = originRefererGlobal
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	body := resp.Body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	var apiResp struct {
		Code int `json:"code"`
		Data struct {
			Models []upstreamModel `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}
	if apiResp.Code != 0 {
		return nil, fmt.Errorf("models API code %d", apiResp.Code)
	}
	var cliModelIDs []string
	for _, a := range apiResp.Data.Agents {
		if a.Name == "cli" {
			cliModelIDs = a.Models
			break
		}
	}
	if len(cliModelIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]upstreamModel, len(apiResp.Data.Models))
	for _, m := range apiResp.Data.Models {
		dynMap[m.ID] = m
	}
	var out []pluginapi.ModelInfo
	for _, id := range cliModelIDs {
		m, ok := dynMap[id]
		if !ok {
			continue
		}
		if m.Disabled {
			continue
		}
		ctxLen := rawNumber(m.MaxInputTokens, m.ContextWindow)
		maxTok := rawNumber(m.MaxOutputTokens, m.MaxTokens)
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.ID,
			Name:                       m.Name,
			ContextLength:              ctxLen,
			MaxCompletionTokens:        maxTok,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out, nil
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
// the workbuddy provider. The host passes this config via HostConfigSummary.
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
	models := wbModels()
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
