package plugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// managementBase is the route namespace this plugin owns. The host normalizes
// paths under /v0/management; conflicts with native routes are skipped by the
// host, so the plugin-id segment is part of every path on purpose.
const managementBase = "/v0/management/plugins/" + PluginID

// resourceBase hosts browser-navigable plugin pages. The host serves /ui
// without authentication (the HTML is public); the API calls the page makes
// carry the management key themselves.
const resourceBase = "/v0/resource/plugins/" + PluginID

func managementRegistration() ManagementRegistration {
	return ManagementRegistration{
		Routes: []ManagementRoute{
			{
				Method:      "GET",
				Path:        managementBase + "/rules",
				Description: "View model visibility rules",
			},
			{
				Method:      "GET",
				Path:        managementBase + "/check",
				Description: "Simulate the catalog a key would see",
			},
		},
		Resources: []ResourceRoute{
			{
				Path:        resourceBase + "/ui",
				Menu:        "模型可见性",
				Description: "Per-key /v1/models catalog visibility",
			},
		},
	}
}

func (a *App) handleManagement(request []byte) []byte {
	var req ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope(400, "invalid_request", "decode management request: "+err.Error())
	}

	path := strings.TrimSuffix(strings.TrimSpace(req.Path), "/")
	switch {
	case strings.EqualFold(req.Method, "GET") && path == resourceBase+"/ui":
		return uiResponse()
	case strings.EqualFold(req.Method, "GET") && strings.HasSuffix(path, managementBase+"/rules"):
		return a.managementRules()
	case strings.EqualFold(req.Method, "GET") && strings.HasSuffix(path, managementBase+"/check"):
		return a.managementCheck(req.Query)
	default:
		return errorEnvelope(404, "not_found", "management path "+path+" is not implemented")
	}
}

// rulesView is the read-only projection of the active snapshot.
type rulesView struct {
	Plugin    string     `json:"plugin"`
	Version   string     `json:"version"`
	Default   string     `json:"default"`
	RuleCount int        `json:"rule_count"`
	KeyCount  int        `json:"key_count"`
	Rules     []ruleView `json:"rules"`
}

type ruleView struct {
	Keys         []string `json:"keys"`
	Allow        []string `json:"allow"`
	Deny         []string `json:"deny"`
	AllowOwnedBy []string `json:"allow_owned_by"`
	DenyOwnedBy  []string `json:"deny_owned_by"`
}

func (a *App) managementRules() []byte {
	snap := a.visibility.snapshot()
	view := rulesView{
		Plugin:    PluginID,
		Version:   version,
		Default:   snap.policy,
		RuleCount: len(snap.rules),
		Rules:     make([]ruleView, 0, len(snap.rules)),
	}
	for _, rule := range snap.rules {
		view.KeyCount += len(rule.Keys)
		view.Rules = append(view.Rules, ruleView{
			Keys:         rule.Keys,
			Allow:        rule.Allow,
			Deny:         rule.Deny,
			AllowOwnedBy: rule.AllowOwnedBy,
			DenyOwnedBy:  rule.DenyOwnedBy,
		})
	}
	return managementJSON(200, view)
}

// managementCheck reports which rule a key would hit and with what patterns.
// It deliberately returns rule data, not a catalog: the effective list is a
// plain authenticated GET /v1/models with that key — which is exactly what
// the panel's simulate card does from the browser.
func (a *App) managementCheck(query map[string][]string) []byte {
	values := url.Values(query)
	key := strings.TrimSpace(values.Get("key"))
	if key == "" {
		return managementJSON(400, map[string]any{
			"error": "query parameter \"key\" is required",
		})
	}
	snap := a.visibility.snapshot()
	check := map[string]any{
		"plugin":  PluginID,
		"version": version,
		"key":     previewKey(key),
		"matched": false,
	}
	rule, ok := snap.byKey[key]
	if !ok {
		check["effective"] = snap.policy
		check["note"] = "no rule matches; the default policy applies"
		return managementJSON(200, check)
	}
	check["matched"] = true
	check["effective"] = "filtered"
	check["allow"] = rule.Allow
	check["deny"] = rule.Deny
	check["allow_owned_by"] = rule.AllowOwnedBy
	check["deny_owned_by"] = rule.DenyOwnedBy
	return managementJSON(200, check)
}

func managementJSON(status int, payload any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		return errorEnvelope(500, "plugin_error", fmt.Sprintf("marshal response: %v", err))
	}
	resp := ManagementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope(500, "plugin_error", err.Error())
	}
	return mustOKEnvelope(raw)
}

// mustOKEnvelope wraps pre-serialized JSON into an OK envelope.
func mustOKEnvelope(result json.RawMessage) []byte {
	raw, err := json.Marshal(Envelope{OK: true, Result: result})
	if err != nil {
		return errorEnvelope(500, "plugin_error", err.Error())
	}
	return raw
}
