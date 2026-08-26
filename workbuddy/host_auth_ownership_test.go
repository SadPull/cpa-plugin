package main

import (
	"encoding/json"
	"testing"
)

// QoderWork credential (nested shape, qoder realm, no top-level type) — the
// mirror image: workbuddy must never claim it.
const qwOAuthNested = `{
  "auth": {"accessToken": "dt-x", "refreshToken": "drt-y", "expiresAt": 1893456000, "domain": "qoder.com.cn"},
  "account": {"uid": "qwuid-1"}
}`

const wbOAuthNestedCN = `{
  "auth": {"accessToken": "wb-at", "refreshToken": "wb-rt", "expiresAt": 1893456000, "domain": "www.codebuddy.cn"},
  "account": {"uid": "wbuid-1", "nickname": "wb user"}
}`

const wbOAuthNestedGlobal = `{
  "auth": {"accessToken": "wb-at", "refreshToken": "wb-rt", "expiresAt": 1893456000, "domain": "www.workbuddy.ai"},
  "account": {"uid": "wbuid-2"}
}`

func TestIsWorkbuddyDomain(t *testing.T) {
	for _, d := range []string{"www.codebuddy.cn", "codebuddy.cn", "api.codebuddy.cn", "www.workbuddy.ai", "workbuddy.ai", "copilot.tencent.com", "TENCENT.COM"} {
		if !isWorkbuddyDomain(d) {
			t.Errorf("isWorkbuddyDomain(%q) = false, want true", d)
		}
	}
	for _, d := range []string{"", "qoder.com.cn", "qoder.com", "api.qoder.com.cn"} {
		if isWorkbuddyDomain(d) {
			t.Errorf("isWorkbuddyDomain(%q) = true, want false", d)
		}
	}
}

func TestDomainFromJSON(t *testing.T) {
	if got := domainFromJSON(json.RawMessage(wbOAuthNestedCN)); got != "www.codebuddy.cn" {
		t.Fatalf("nested workbuddy domain: got %q", got)
	}
	if got := domainFromJSON(json.RawMessage(qwOAuthNested)); got != "qoder.com.cn" {
		t.Fatalf("nested qoderwork domain: got %q", got)
	}
}

// Mirror regression: a type-less qoderwork credential must be rejected by the
// workbuddy ownership check once its domain is known; workbuddy's own files
// (CN + Global) must still be accepted.
func TestOwnershipByDomain(t *testing.T) {
	if d := domainFromJSON(json.RawMessage(qwOAuthNested)); isWorkbuddyDomain(d) {
		t.Errorf("qoderwork domain %q must NOT be claimed by workbuddy", d)
	}
	for name, raw := range map[string]string{"cn": wbOAuthNestedCN, "global": wbOAuthNestedGlobal} {
		d := domainFromJSON(json.RawMessage(raw))
		if !isWorkbuddyDomain(d) {
			t.Errorf("%s: workbuddy domain %q must be claimed by workbuddy", name, d)
		}
	}
}

func TestDeclaredTypeFromJSON(t *testing.T) {
	typed := `{"type":"workbuddy","auth":{}}`
	if got := declaredTypeFromJSON(json.RawMessage(typed)); got != "workbuddy" {
		t.Fatalf("typed file: got %q want workbuddy", got)
	}
	if got := declaredTypeFromJSON(json.RawMessage(wbOAuthNestedCN)); got != "" {
		t.Fatalf("type-less workbuddy file should yield empty declared type, got %q", got)
	}
}
