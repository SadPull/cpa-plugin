package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Regression guard for the field-preservation fix: every plugin rewrite path
// must keep user-managed top-level fields (excluded-models, prefix, proxy_url,
// priority, headers, panel-set metadata) instead of rebuilding the file from a
// fixed 7-key shape. Root cause of the "OAuth 模型禁用对 workbuddy 无效" bug:
// syncAuthNote / reenableAuth / disable / token-refresh all rebuilt the file
// with buildAuthFileJSON (7 fixed keys) and silently dropped the field.

func sampleStoredAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1, Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u-1", Nickname: "n"},
	}
}

func existingFileWithUserFields() []byte {
	return []byte(`{
		"type": "workbuddy",
		"provider": "workbuddy",
		"logo": "old-logo",
		"disabled": false,
		"note": "old note",
		"excluded-models": ["hy3", "kimi-k2.6"],
		"prefix": "wb",
		"priority": 10,
		"proxy_url": "socks5://u:p@1.2.3.4:1080/",
		"headers": {"X-Custom": "v"},
		"auth": {"accessToken": "old", "refreshToken": "oldr", "expiresAt": 1, "domain": "www.codebuddy.cn"},
		"account": {"uid": "u-1", "nickname": "n"}
	}`)
}

func TestBuildAuthFileJSONPreserve_KeepsUserFields(t *testing.T) {
	raw, err := buildAuthFileJSONPreserve(existingFileWithUserFields(), sampleStoredAuth(), true, "new note", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	// User-managed fields must survive.
	if got, ok := out["excluded-models"].([]any); !ok || len(got) != 2 {
		t.Fatalf("excluded-models lost or wrong: %v", out["excluded-models"])
	}
	if out["prefix"] != "wb" {
		t.Fatalf("prefix lost: %v", out["prefix"])
	}
	if out["proxy_url"] != "socks5://u:p@1.2.3.4:1080/" {
		t.Fatalf("proxy_url lost: %v", out["proxy_url"])
	}
	if p, ok := out["priority"].(float64); !ok || p != 10 {
		t.Fatalf("priority lost: %v", out["priority"])
	}
	if h, ok := out["headers"].(map[string]any); !ok || h["X-Custom"] != "v" {
		t.Fatalf("headers lost: %v", out["headers"])
	}
	// Plugin-owned keys must be updated.
	if out["note"] != "new note" {
		t.Fatalf("note not updated: %v", out["note"])
	}
	if out["disabled"] != true {
		t.Fatalf("disabled not updated: %v", out["disabled"])
	}
	if out["type"] != providerName {
		t.Fatalf("type: %v", out["type"])
	}
	if out["logo"] != pluginLogoURL {
		t.Fatalf("logo not refreshed: %v", out["logo"])
	}
	// Nested credential blocks must come from sa (new tokens), not the file.
	auth, _ := out["auth"].(map[string]any)
	if auth["accessToken"] != "a" {
		t.Fatalf("auth.accessToken not from sa: %v", auth["accessToken"])
	}
	acct, _ := out["account"].(map[string]any)
	if acct["uid"] != "u-1" {
		t.Fatalf("account.uid: %v", acct["uid"])
	}
}

func TestBuildAuthFileJSONPreserve_EmptyCurrent_FallsBackToFixedShape(t *testing.T) {
	for _, current := range [][]byte{nil, {}, []byte(""), []byte("not-json")} {
		raw, err := buildAuthFileJSONPreserve(current, sampleStoredAuth(), false, "note", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out["type"] != providerName || out["note"] != "note" {
			t.Fatalf("fixed shape missing for current=%q: %v", current, out)
		}
		if _, ok := out["auth"]; !ok {
			t.Fatalf("auth block missing for current=%q", current)
		}
	}
}

func TestEnrichAuthMetadata_PreservesExistingKeys(t *testing.T) {
	existing := map[string]any{
		"excluded-models": []any{"hy3"},
		"prefix":          "wb",
		"priority":        float64(10),
		"type":            "stale-type", // must be overwritten
		"note":            "stale-note", // must be overwritten
	}
	meta := enrichAuthMetadata(sampleStoredAuth(), nil, false, existing)
	if got, ok := meta["excluded-models"].([]any); !ok || len(got) != 1 {
		t.Fatalf("excluded-models lost: %v", meta["excluded-models"])
	}
	if meta["prefix"] != "wb" {
		t.Fatalf("prefix lost: %v", meta["prefix"])
	}
	if meta["type"] != providerName {
		t.Fatalf("type not overwritten: %v", meta["type"])
	}
	if meta["note"] == "stale-note" || !strings.Contains(meta["note"].(string), "积分") {
		t.Fatalf("note not rebuilt: %v", meta["note"])
	}
	if meta["disabled"] != false {
		t.Fatalf("disabled: %v", meta["disabled"])
	}
}

func TestRefreshMetadataBase_StripsNestedKeys(t *testing.T) {
	// Unit-test environment has no host RPC (hostAPI nil), so the physical-file
	// lookup fails and refreshMetadataBase must fall back to req.Metadata —
	// while still stripping the nested credential blocks (they belong to
	// StorageJSON, not Metadata) and keeping user-managed keys.
	req := pluginapi.AuthRefreshRequest{
		AuthID: "workbuddy-u-1.json",
		Metadata: map[string]any{
			"excluded-models": []any{"hy3"},
			"auth":            map[string]any{"accessToken": "x"},
			"account":         map[string]any{"uid": "u"},
			"note":            "n",
		},
	}
	base := refreshMetadataBase(req)
	if base == nil {
		t.Fatal("nil base")
	}
	if _, ok := base["auth"]; ok {
		t.Fatal("auth leaked into metadata base")
	}
	if _, ok := base["account"]; ok {
		t.Fatal("account leaked into metadata base")
	}
	if got, ok := base["excluded-models"].([]any); !ok || len(got) != 1 {
		t.Fatalf("excluded-models lost: %v", base["excluded-models"])
	}
	if base["note"] != "n" {
		t.Fatalf("note lost: %v", base["note"])
	}
}

func TestRefreshMetadataBase_NilInput(t *testing.T) {
	if base := refreshMetadataBase(pluginapi.AuthRefreshRequest{}); base != nil {
		t.Fatalf("expected nil base, got %v", base)
	}
}

func TestToAuthDataForRefresh_PreservesMetadata(t *testing.T) {
	ad := toAuthDataForRefresh(sampleStoredAuth(), map[string]any{
		"excluded-models": []any{"hy3"},
		"prefix":          "wb",
	})
	if ad.Metadata == nil {
		t.Fatal("nil metadata")
	}
	if got, ok := ad.Metadata["excluded-models"].([]any); !ok || len(got) != 1 {
		t.Fatalf("excluded-models lost in AuthData.Metadata: %v", ad.Metadata["excluded-models"])
	}
	if ad.Metadata["prefix"] != "wb" {
		t.Fatalf("prefix lost: %v", ad.Metadata["prefix"])
	}
	if ad.Metadata["type"] != providerName {
		t.Fatalf("type: %v", ad.Metadata["type"])
	}
}

// TestHandleParseAuth_PreservesUserFields is the regression guard for the
// PRIMARY model-disable bug: the host persists an auth as
// mergedStorageJSON(StorageJSON, Metadata), so whatever metadata ParseAuth
// returns becomes the file's top-level keys on the next watcher re-parse.
// Returning only the plugin's 5 keys made every re-parse rewrite the file
// WITHOUT excluded-models, silently undoing the panel's model-disable within
// the same second (observed live at 2026-09-06 22:53:40). ParseAuth must now
// carry the file's user fields through into AuthData.Metadata.
func TestHandleParseAuth_PreservesUserFields(t *testing.T) {
	uid := "a7eb4760-9486-47c8-8979-8f27a08613b4"
	raw := []byte(`{
		"type": "workbuddy",
		"provider": "workbuddy",
		"disabled": false,
		"note": "CN · 积分未知",
		"excluded-models": ["hy3", "kimi-k2.6"],
		"prefix": "wb",
		"priority": 5,
		"auth": {"accessToken":"a","refreshToken":"r","expiresAt":1,"domain":"www.codebuddy.cn"},
		"account": {"uid":"` + uid + `","nickname":"n"}
	}`)
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/root/.cli-proxy-api/workbuddy-" + uid + ".json",
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  raw,
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("workbuddy file not handled")
	}
	meta := resp.Auth.Metadata
	if got, ok := meta["excluded-models"].([]any); !ok || len(got) != 2 {
		t.Fatalf("excluded-models lost through ParseAuth: %v", meta["excluded-models"])
	}
	if meta["prefix"] != "wb" {
		t.Fatalf("prefix lost: %v", meta["prefix"])
	}
	if p, ok := meta["priority"].(float64); !ok || p != 5 {
		t.Fatalf("priority lost: %v", meta["priority"])
	}
	// Plugin-owned keys still normalized.
	if meta["type"] != providerName || meta["provider"] != providerName {
		t.Fatalf("type/provider: %v %v", meta["type"], meta["provider"])
	}
	// Nested credential blocks must NOT be duplicated into Metadata.
	if _, ok := meta["auth"]; ok {
		t.Fatal("auth leaked into ParseAuth metadata")
	}
	if _, ok := meta["account"]; ok {
		t.Fatal("account leaked into ParseAuth metadata")
	}
}

// TestHandleParseAuth_HonorsFileDisabled guards the secondary parse bug: a
// re-parse must not resurrect an account the lifecycle disabled (disabled:true
// on disk must survive into AuthData.Disabled).
func TestHandleParseAuth_HonorsFileDisabled(t *testing.T) {
	uid := "a7eb4760-9486-47c8-8979-8f27a08613b4"
	raw := []byte(`{
		"type": "workbuddy",
		"disabled": true,
		"note": "CN · 已禁用",
		"auth": {"accessToken":"a","refreshToken":"r","expiresAt":1,"domain":"www.codebuddy.cn"},
		"account": {"uid":"` + uid + `"}
	}`)
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  raw,
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Auth.Disabled {
		t.Fatal("disabled:true on file was lost — parse would resurrect a disabled account")
	}
	if resp.Auth.Metadata["disabled"] != true {
		t.Fatalf("disabled not reflected in metadata: %v", resp.Auth.Metadata["disabled"])
	}
}

func TestUserFieldsFromAuthJSON(t *testing.T) {
	got := userFieldsFromAuthJSON(existingFileWithUserFields())
	if got == nil {
		t.Fatal("nil")
	}
	if _, ok := got["auth"]; ok {
		t.Fatal("auth must be stripped")
	}
	if _, ok := got["account"]; ok {
		t.Fatal("account must be stripped")
	}
	if _, ok := got["excluded-models"]; !ok {
		t.Fatal("excluded-models must be kept")
	}
	if userFieldsFromAuthJSON(nil) != nil {
		t.Fatal("nil input should give nil")
	}
	if userFieldsFromAuthJSON([]byte(`{"auth":{},"account":{}}`)) != nil {
		t.Fatal("only-nested should give nil")
	}
}
