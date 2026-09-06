package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Regression guard for the field-preservation fix (mirrors
// workbuddy/field_preservation_test.go): every plugin rewrite path must keep
// user-managed top-level fields (excluded-models, prefix, proxy_url, priority,
// headers) instead of rebuilding the auth file from a fixed 7-key shape.

func sampleStoredAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "jt-a", RefreshToken: "jrt-r", ExpiresAt: 1, Domain: "qoder.com.cn"},
		Account: storedAccount{UID: "u-1", Nickname: "n"},
	}
}

// decodeParseAuth unwraps the plugin RPC envelope into an AuthParseResponse
// (qoderwork has no auth_identity_test.go, so the helper lives here).
func decodeParseAuth(t *testing.T, raw []byte) pluginapi.AuthParseResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
	var resp pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	return resp
}

func existingFileWithUserFields() []byte {	return []byte(`{
		"type": "qoderwork",
		"provider": "qoderwork",
		"logo": "old-logo",
		"disabled": false,
		"note": "old note",
		"excluded-models": ["qmodel_preview", "qmodel_legacy"],
		"prefix": "qw",
		"priority": 10,
		"proxy_url": "socks5://u:p@1.2.3.4:1080/",
		"headers": {"X-Custom": "v"},
		"auth": {"accessToken": "old", "refreshToken": "oldr", "expiresAt": 1, "domain": "qoder.com.cn"},
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
	if got, ok := out["excluded-models"].([]any); !ok || len(got) != 2 {
		t.Fatalf("excluded-models lost or wrong: %v", out["excluded-models"])
	}
	if out["prefix"] != "qw" {
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
	if out["note"] != "new note" {
		t.Fatalf("note not updated: %v", out["note"])
	}
	if out["disabled"] != true {
		t.Fatalf("disabled not updated: %v", out["disabled"])
	}
	if out["type"] != providerName || out["provider"] != providerName {
		t.Fatalf("type/provider: %v %v", out["type"], out["provider"])
	}
	if out["logo"] != pluginLogoURL {
		t.Fatalf("logo not refreshed: %v", out["logo"])
	}
	auth, _ := out["auth"].(map[string]any)
	if auth["accessToken"] != "jt-a" {
		t.Fatalf("auth.accessToken not from sa: %v", auth["accessToken"])
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
		"excluded-models": []any{"qmodel_preview"},
		"prefix":          "qw",
		"priority":        float64(10),
		"type":            "stale-type",
		"note":            "stale-note",
	}
	meta := enrichAuthMetadata(sampleStoredAuth(), nil, false, existing)
	if got, ok := meta["excluded-models"].([]any); !ok || len(got) != 1 {
		t.Fatalf("excluded-models lost: %v", meta["excluded-models"])
	}
	if meta["prefix"] != "qw" {
		t.Fatalf("prefix lost: %v", meta["prefix"])
	}
	if meta["type"] != providerName {
		t.Fatalf("type not overwritten: %v", meta["type"])
	}
	if note, _ := meta["note"].(string); note == "stale-note" || !strings.Contains(note, "CN") {
		t.Fatalf("note not rebuilt: %v", meta["note"])
	}
	if meta["disabled"] != false {
		t.Fatalf("disabled: %v", meta["disabled"])
	}
}

func TestRefreshMetadataBase_StripsNestedKeys(t *testing.T) {
	// Unit-test environment has no host RPC (hostAPI nil), so the physical-file
	// lookup fails and refreshMetadataBase must fall back to req.Metadata —
	// while stripping nested credential blocks and keeping user-managed keys.
	req := pluginapi.AuthRefreshRequest{
		AuthID: "qoderwork-u-1.json",
		Metadata: map[string]any{
			"excluded-models": []any{"qmodel_preview"},
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
		"excluded-models": []any{"qmodel_preview"},
		"prefix":          "qw",
	})
	if ad.Metadata == nil {
		t.Fatal("nil metadata")
	}
	if got, ok := ad.Metadata["excluded-models"].([]any); !ok || len(got) != 1 {
		t.Fatalf("excluded-models lost in AuthData.Metadata: %v", ad.Metadata["excluded-models"])
	}
	if ad.Metadata["prefix"] != "qw" {
		t.Fatalf("prefix lost: %v", ad.Metadata["prefix"])
	}
	if ad.Metadata["type"] != providerName {
		t.Fatalf("type: %v", ad.Metadata["type"])
	}
	if ad.FileName != "" || ad.ID != "" {
		t.Fatalf("FileName/ID must stay empty for host backfill: %q %q", ad.FileName, ad.ID)
	}
}

// TestHandleParseAuth_PreservesUserFields is the regression guard for the
// PRIMARY model-disable bug (mirrors workbuddy): the host persists an auth as
// mergedStorageJSON(StorageJSON, Metadata), so whatever metadata ParseAuth
// returns becomes the file's top-level keys on the next watcher re-parse.
// Returning only the plugin's 5 keys made every re-parse rewrite the file
// WITHOUT excluded-models. ParseAuth must carry user fields through.
func TestHandleParseAuth_PreservesUserFields(t *testing.T) {
	uid := "019eedc2-d993-7afc-82ee-ed182980ae6b"
	raw := []byte(`{
		"type": "qoderwork",
		"provider": "qoderwork",
		"disabled": false,
		"note": "CN · 积分未知",
		"excluded-models": ["qmodel_preview", "qmodel_legacy"],
		"prefix": "qw",
		"priority": 5,
		"auth": {"accessToken":"jt-a","refreshToken":"jrt-r","expiresAt":1,"domain":"qoder.com.cn"},
		"account": {"uid":"` + uid + `","nickname":"n"}
	}`)
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/root/.cli-proxy-api/qoderwork-" + uid + ".json",
		FileName: "qoderwork-" + uid + ".json",
		RawJSON:  raw,
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("qoderwork file not handled")
	}
	meta := resp.Auth.Metadata
	if got, ok := meta["excluded-models"].([]any); !ok || len(got) != 2 {
		t.Fatalf("excluded-models lost through ParseAuth: %v", meta["excluded-models"])
	}
	if meta["prefix"] != "qw" {
		t.Fatalf("prefix lost: %v", meta["prefix"])
	}
	if p, ok := meta["priority"].(float64); !ok || p != 5 {
		t.Fatalf("priority lost: %v", meta["priority"])
	}
	if meta["type"] != providerName || meta["provider"] != providerName {
		t.Fatalf("type/provider: %v %v", meta["type"], meta["provider"])
	}
	if _, ok := meta["auth"]; ok {
		t.Fatal("auth leaked into ParseAuth metadata")
	}
	if _, ok := meta["account"]; ok {
		t.Fatal("account leaked into ParseAuth metadata")
	}
}

func TestHandleParseAuth_HonorsFileDisabled(t *testing.T) {
	uid := "019eedc2-d993-7afc-82ee-ed182980ae6b"
	raw := []byte(`{
		"type": "qoderwork",
		"disabled": true,
		"note": "CN · 已禁用",
		"auth": {"accessToken":"jt-a","refreshToken":"jrt-r","expiresAt":1,"domain":"qoder.com.cn"},
		"account": {"uid":"` + uid + `"}
	}`)
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "qoderwork-" + uid + ".json",
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
