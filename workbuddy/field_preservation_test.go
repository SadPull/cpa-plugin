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
