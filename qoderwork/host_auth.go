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

// hostAuthList returns all qoderwork credentials known to the host.
//
// Ownership is decided by CREDENTIAL CONTENT, not by the host's classification:
// the host labels type-less files by whichever plugin's ParseAuth claims them
// first, and that label has been observed to flip a workbuddy OAuth file to
// qoderwork (both plugins share an identical nested {auth,account} shape, so
// qoderwork's parseStored accepts a workbuddy token). Once the host relabels
// it, a naive "qoderwork-" filename / Type==qoderwork filter would hand the
// workbuddy credential to qoderwork's keepalive / reconcile / models list,
// which then call qoder.com.cn with a codebuddy.cn token — the reported bug.
//
// The list RPC does not return file bodies, so we classify cheaply first and
// only host.auth.get the ambiguous ones (type-less + not obviously foreign).
// Rules (first match wins):
//  1. Top-level "type"/"provider" on the entry → trust it (exact match).
//  2. Entry lacks a type → fetch raw JSON, re-check the file's own type, then
//     fall back to auth.domain: qoder.com.cn/qoder.com → ours;
//     codebuddy.cn/workbuddy.ai → theirs.
//  3. Still inconclusive → legacy "qoderwork-" filename prefix.
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

// hostAuthEntryOurs decides whether one host auth entry is a qoderwork
// credential. Type/provider on the entry is authoritative; otherwise we look
// at the raw file content (type, then auth.domain) via one host.auth.get.
//
// Type-less entries each cost one host.auth.get, and hostAuthList is called
// on every keepalive / reconcile / checkin / panel / models pass, so the
// content verdict is cached per (AuthIndex, ModTime) — a file rewrite bumps
// ModTime and forces re-classification, keeping the cache coherent.
func hostAuthEntryOurs(f *pluginapi.HostAuthFileEntry) bool {
	declared := strings.ToLower(strings.TrimSpace(f.Type))
	if declared == "" {
		declared = strings.ToLower(strings.TrimSpace(f.Provider))
	}
	if declared != "" {
		// Host already classified it. Trust an explicit qoderwork label, and
		// trust an explicit FOREIGN label too (don't second-guess "workbuddy").
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
	raw, err := hostAuthGetByIndex(f.AuthIndex)
	if err == nil && len(raw) > 0 {
		if t := declaredTypeFromJSON(raw); t != "" {
			return t == providerName
		}
		if d := domainFromJSON(raw); d != "" {
			return isQoderDomain(d)
		}
	}
	// Inconclusive content — fall back to the historical filename rule.
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.Name)), providerName+"-")
}

// ownershipCache maps AuthIndex → cached content verdict. hostAuthPhysical is
// too heavy to keep here; the bool + ModTime stamp is enough.
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
	// Only trust the cache when the file hasn't changed since we classified it.
	if !e.modTime.Equal(modTime) {
		return false, false
	}
	return e.ours, true
}

func rememberOwnershipVerdict(authIndex string, modTime time.Time, ours bool) {
	ownershipCache.Store(authIndex, ownershipVerdictEntry{modTime: modTime, ours: ours})
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

// isQoderDomain reports whether a domain belongs to the QoderWork service.
func isQoderDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	return d == "qoder.com.cn" || d == "qoder.com" ||
		strings.HasSuffix(d, ".qoder.com.cn") || strings.HasSuffix(d, ".qoder.com")
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
