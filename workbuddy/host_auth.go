// host_auth.go wraps the host's auth-store RPC (host.auth.list / get /
// get_bundle). These are the only paths the plugin uses to read auth files;
// writes go through hostAuthPersist / hostAuthPersistMigrate in lifecycle.go.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all workbuddy credentials known to the host.
//
// Ownership is decided by CREDENTIAL CONTENT, not by the host's classification:
// the host labels type-less files by whichever plugin's ParseAuth claims them
// first, and qoderwork's ParseAuth can claim a workbuddy file (both plugins
// share the nested {auth,account} shape). A naive "workbuddy-" filename /
// Type==workbuddy filter would then hand a qoderwork credential to workbuddy's
// reconcile / models / scheduler — the mirror image of the reported bug.
//
// The list RPC does not return file bodies, so we classify cheaply first and
// only host.auth.get the ambiguous (type-less) ones. Rules (first match wins):
//  1. Top-level "type"/"provider" on the entry → trust it (exact match).
//  2. Entry lacks a type → fetch raw JSON, re-check the file's own type, then
//     fall back to auth.domain: codebuddy.cn/workbuddy.ai → ours;
//     qoder.com.cn/qoder.com → theirs.
//  3. Still inconclusive → legacy "workbuddy-" filename prefix.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	// Fresh slice — resp.Files[:0] would alias the RPC response's backing
	// array (P1-3: fragile pattern, safe today but could break if resp is
	// ever cached/reused).
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	for _, f := range resp.Files {
		if hostAuthEntryOurs(&f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// hostAuthEntryOurs decides whether one host auth entry is a workbuddy
// credential. Type/provider on the entry is authoritative; otherwise we look
// at the raw file content (type, then auth.domain) via one host.auth.get.
func hostAuthEntryOurs(f *pluginapi.HostAuthFileEntry) bool {
	declared := strings.ToLower(strings.TrimSpace(f.Type))
	if declared == "" {
		declared = strings.ToLower(strings.TrimSpace(f.Provider))
	}
	if declared != "" {
		// Host already classified it. Trust an explicit workbuddy label, and
		// trust an explicit FOREIGN label too (don't second-guess "qoderwork").
		return declared == providerName
	}
	// Type-less entry: content is the source of truth (cached per ModTime).
	if ours, ok := ownershipVerdict(f.AuthIndex, f.ModTime); ok {
		return ours
	}
	ours := classifyTypelessEntry(f)
	rememberOwnershipVerdict(f.AuthIndex, f.ModTime, ours)
	return ours
}

// classifyTypelessEntry does the (uncached) content classification for an
// entry with no type/provider on it: fetch the file, check its own type, then
// its domain, then the legacy filename prefix.
func classifyTypelessEntry(f *pluginapi.HostAuthFileEntry) bool {
	raw, err := hostAuthGetRaw(f.AuthIndex)
	if err == nil && len(raw) > 0 {
		if t := declaredTypeFromJSON(raw); t != "" {
			return t == providerName
		}
		if d := domainFromJSON(raw); d != "" {
			return isWorkbuddyDomain(d)
		}
	}
	// Inconclusive content — fall back to the historical filename rule.
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.Name)), providerName+"-")
}

// ownershipCache maps AuthIndex → cached content verdict, keyed by ModTime so
// a file rewrite forces re-classification (hostAuthList is called on every
// reconcile / scheduler / panel pass, so uncached would be N+1 RPC per pass).
var ownershipCache sync.Map // authIndex -> ownershipVerdictEntry

type ownershipVerdictEntry struct {
	modTime time.Time
	ours    bool
}

func ownershipVerdict(authIndex string, modTime time.Time) (bool, bool) {
	v, ok := ownershipCache.Load(authIndex)
	if !ok {
		return false, false
	}
	e := v.(ownershipVerdictEntry)
	if !e.modTime.Equal(modTime) {
		return false, false
	}
	return e.ours, true
}

func rememberOwnershipVerdict(authIndex string, modTime time.Time, ours bool) {
	ownershipCache.Store(authIndex, ownershipVerdictEntry{modTime: modTime, ours: ours})
}

// hostAuthGetRaw returns the raw auth JSON for one auth index.
func hostAuthGetRaw(authIndex string) (json.RawMessage, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return phys.JSON, nil
}

// declaredTypeFromJSON reads the top-level "type"/"provider" from raw auth JSON.
func declaredTypeFromJSON(rawJSON json.RawMessage) string {
	if len(rawJSON) == 0 {
		return ""
	}
	var probe struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(rawJSON, &probe); err != nil {
		return ""
	}
	if s := strings.ToLower(strings.TrimSpace(probe.Type)); s != "" {
		return s
	}
	return strings.ToLower(strings.TrimSpace(probe.Provider))
}

// domainFromJSON reads auth.domain (nested shape) or top-level domain (flat).
func domainFromJSON(rawJSON json.RawMessage) string {
	if len(rawJSON) == 0 {
		return ""
	}
	var probe struct {
		Auth struct {
			Domain string `json:"domain"`
		} `json:"auth"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(rawJSON, &probe); err != nil {
		return ""
	}
	if s := strings.ToLower(strings.TrimSpace(probe.Auth.Domain)); s != "" {
		return s
	}
	return strings.ToLower(strings.TrimSpace(probe.Domain))
}

// isWorkbuddyDomain reports whether a domain belongs to the WorkBuddy service
// (CN: www.codebuddy.cn / copilot.tencent.com; Global: www.workbuddy.ai).
func isWorkbuddyDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	if isGlobalDomain(d) {
		return true
	}
	return d == "codebuddy.cn" || strings.HasSuffix(d, ".codebuddy.cn") ||
		d == "tencent.com" || strings.HasSuffix(d, ".tencent.com")
}

// hostAuthGet fetches the credential JSON for one auth index.
func hostAuthGet(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// hostAuthGetBundle is one host.auth.get for both storage and physical metadata
// (avoids the previous double-RPC in dashboard: get + getPhysical).
func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}
