package main

import (
	"encoding/json"
	"testing"
)

// WorkBuddy OAuth credential (nested shape, CN realm, NO top-level type) as
// seen in the wild — the file the host used to mislabel as qoderwork.
const wbOAuthNestedCN = `{
  "auth": {"accessToken": "wb-at", "refreshToken": "wb-rt", "expiresAt": 1893456000, "domain": "www.codebuddy.cn"},
  "account": {"uid": "wbuid-1", "nickname": "wb user"}
}`

const wbOAuthNestedGlobal = `{
  "auth": {"accessToken": "wb-at", "refreshToken": "wb-rt", "expiresAt": 1893456000, "domain": "www.workbuddy.ai"},
  "account": {"uid": "wbuid-2"}
}`

const qwOAuthNested = `{
  "auth": {"accessToken": "dt-x", "refreshToken": "drt-y", "expiresAt": 1893456000, "domain": "qoder.com.cn"},
  "account": {"uid": "qwuid-1"}
}`

func TestDeclaredTypeFromJSON(t *testing.T) {
	typed := `{"type":"qoderwork","provider":"qoderwork","auth":{}}`
	if got := declaredTypeFromJSON(json.RawMessage(typed)); got != "qoderwork" {
		t.Fatalf("typed file: got %q want qoderwork", got)
	}
	foreign := `{"type":"workbuddy","auth":{}}`
	if got := declaredTypeFromJSON(json.RawMessage(foreign)); got != "workbuddy" {
		t.Fatalf("foreign typed file: got %q want workbuddy", got)
	}
	if got := declaredTypeFromJSON(json.RawMessage(wbOAuthNestedCN)); got != "" {
		t.Fatalf("type-less workbuddy file should yield empty declared type, got %q", got)
	}
}

func TestDomainFromJSON(t *testing.T) {
	if got := domainFromJSON(json.RawMessage(wbOAuthNestedCN)); got != "www.codebuddy.cn" {
		t.Fatalf("nested workbuddy domain: got %q", got)
	}
	if got := domainFromJSON(json.RawMessage(qwOAuthNested)); got != "qoder.com.cn" {
		t.Fatalf("nested qoderwork domain: got %q", got)
	}
	flat := `{"accessToken":"x","domain":"qoder.com.cn"}`
	if got := domainFromJSON(json.RawMessage(flat)); got != "qoder.com.cn" {
		t.Fatalf("flat qoderwork domain: got %q", got)
	}
}

func TestIsQoderDomain(t *testing.T) {
	for _, d := range []string{"qoder.com.cn", "qoder.com", "api.qoder.com.cn", "OpenAPI.Qoder.COM"} {
		if !isQoderDomain(d) {
			t.Errorf("isQoderDomain(%q) = false, want true", d)
		}
	}
	for _, d := range []string{"", "www.codebuddy.cn", "codebuddy.cn", "www.workbuddy.ai", "workbuddy.ai", "copilot.tencent.com"} {
		if isQoderDomain(d) {
			t.Errorf("isQoderDomain(%q) = true, want false", d)
		}
	}
}

// The core regression: a type-less workbuddy credential (CN or Global) must be
// rejected by the qoderwork ownership check once its domain is known.
func TestWorkbuddyCredentialRejectedByDomain(t *testing.T) {
	for name, raw := range map[string]string{"cn": wbOAuthNestedCN, "global": wbOAuthNestedGlobal} {
		d := domainFromJSON(json.RawMessage(raw))
		if d == "" {
			t.Fatalf("%s: expected a domain to be extracted", name)
		}
		if isQoderDomain(d) {
			t.Errorf("%s: workbuddy domain %q must NOT be claimed by qoderwork", name, d)
		}
		if declaredTypeFromJSON(json.RawMessage(raw)) == "qoderwork" {
			t.Errorf("%s: type-less workbuddy file must not read as qoderwork", name)
		}
	}
}
