// authfile.go owns every physical auth-file path the plugin touches: the
// workbuddy-<uid>.json naming rule, UID sanitization (path-traversal defense),
// path safety checks, and the read / write / delete helpers that talk to the
// host's auth store via host.auth.* RPC. Callers above (lifecycle reconcile)
// decide when to disable / re-enable / delete; this file decides how.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authFileNameFor matches toAuthData naming: always workbuddy-<uid>.json when UID is known.
// Bare "workbuddy.json" is legacy single-account only (no UID).
var unsafeUIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	uid = unsafeUIDChars.ReplaceAllString(uid, "_")
	if uid == "" || uid == "." || uid == ".." {
		return ""
	}
	if len(uid) > 64 {
		uid = uid[:64]
	}
	return uid
}

func authFileNameFor(sa *storedAuth) string {
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			return "workbuddy-" + uid + ".json"
		}
	}
	return authFileName
}

// isLegacyWorkbuddyAuthName reports the historical single-file name that collides
// with multi-account workbuddy-<uid>.json for the same credential.

func isLegacyWorkbuddyAuthName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), authFileName)
}

// resolveAuthFileTarget picks the canonical file name + path for save/delete.
// Prefer workbuddy-<uid>.json; if the host still points at legacy workbuddy.json
// for a UID-bearing account, rewrite to the uid name and schedule legacy removal.

func resolveAuthFileTarget(sa *storedAuth, phys *hostAuthPhysical) (name, path string, legacyPath string) {
	name = authFileNameFor(sa)
	if phys != nil {
		path = strings.TrimSpace(phys.Path)
		physName := strings.TrimSpace(phys.Name)
		if physName != "" && !isLegacyWorkbuddyAuthName(physName) {
			// Already on multi-account name — keep host name (should match uid form).
			name = physName
		}
		if isLegacyWorkbuddyAuthName(physName) || isLegacyWorkbuddyAuthName(filepath.Base(path)) {
			if sa != nil && strings.TrimSpace(sa.Account.UID) != "" {
				// Migrate: write canonical, delete legacy path after save.
				legacyPath = path
				if isLegacyWorkbuddyAuthName(filepath.Base(path)) {
					// path stays legacy until we write canonical beside it
				}
				// After persist to name, remove legacyPath if different.
			}
		}
	}
	return name, path, legacyPath
}

// hostAuthPersist saves via host API only. Dual-writing the physical path after
// a successful host.auth.save is redundant (host already WriteFile) and can
// re-fire the watcher → extra re-parse / transient dual registration risk.

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

// hostAuthSaveJSON persists credential JSON via host.auth.save.

func hostAuthPersist(name, path string, raw []byte) error {
	_ = path // reserved for callers that still pass physical path for migrate logic
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	return hostAuthSaveJSON(name, raw)
}

// hostAuthPersistMigrate is like hostAuthPersist but also removes a legacy path
// when the canonical name differs (workbuddy.json → workbuddy-<uid>.json).

func hostAuthPersistMigrate(name, path, legacyPath string, raw []byte) error {
	if err := hostAuthPersist(name, path, raw); err != nil {
		return err
	}
	// If path was legacy and name is canonical, also write canonical path next to it.
	if legacyPath != "" && !strings.EqualFold(filepath.Base(legacyPath), name) {
		// host.auth.save already wrote name under auth dir; drop legacy file.
		// A-36: use deleteAuthFileInDir (abs path + dir confine) for consistency.
		if isLegacyWorkbuddyAuthName(filepath.Base(legacyPath)) {
			_ = deleteAuthFileInDir(legacyPath, filepath.Dir(legacyPath))
		}
	}
	// If path points at legacy but name is uid form, do not dual-write path (would keep legacy alive).
	return nil
}

// buildAuthFileJSON produces host-save payload: nested storage + top-level metadata.
// extra merges additional top-level keys (optional).

func hostAuthSaveJSON(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: raw,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = truncateRedacted(env.Error.Message, 200)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// lifecycleStateUnchanged avoids redundant saves when note/disabled unchanged.

// buildAuthFileJSON produces host-save payload: nested storage + top-level metadata.
// extra merges additional top-level keys (optional).
//
// The output is built from a FIXED 7-key shape, so any top-level field the
// user (or the management panel) added to the auth file — excluded-models,
// prefix, proxy_url, priority, headers, note overrides, etc. — is silently
// dropped whenever the plugin rewrites the file. Callers that rewrite an
// EXISTING file must use buildAuthFileJSONPreserve instead (round-trips the
// current physical JSON). This fixed-shape builder is only appropriate for
// creating a brand-new credential file (e.g. import / fresh login).

func buildAuthFileJSON(sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	out := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"logo":     pluginLogoURL,
		"disabled": disabled,
		"note":     note,
		"auth":     nested["auth"],
		"account":  nested["account"],
	}
	for k, v := range extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// buildAuthFileJSONPreserve rewrites an EXISTING auth file without dropping
// user-managed top-level fields. It starts from the current physical JSON
// (current), overwrites only the 7 keys the plugin owns
// (type/provider/logo/disabled/note/auth/account), then applies extra.
// Unknown keys — excluded-models, prefix, proxy_url, priority, headers and
// any panel-set metadata — pass through untouched. If current is empty or
// not a JSON object (e.g. first-ever write), it behaves like buildAuthFileJSON.
//
// This mirrors stampAuthJSON's map[string]json.RawMessage round-trip so that
// "no field — known or user-added — is lost" holds for every rewrite path,
// not just the models-catalog push.

func buildAuthFileJSONPreserve(current []byte, sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	base := map[string]any{}
	if err := json.Unmarshal(current, &base); err != nil || base == nil {
		base = map[string]any{}
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	base["type"] = providerName
	base["provider"] = providerName
	base["logo"] = pluginLogoURL
	base["disabled"] = disabled
	base["note"] = note
	base["auth"] = nested["auth"]
	base["account"] = nested["account"]
	for k, v := range extra {
		base[k] = v
	}
	return json.Marshal(base)
}

// parseDisabledFromAuthJSON reads top-level disabled from physical auth JSON.

func parseDisabledFromAuthJSON(raw []byte) bool {
	var m struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Disabled
}

// userFieldsFromAuthJSON extracts the user-managed top-level fields from a
// physical auth file, for use as a metadata base. The host persists an auth
// as mergedStorageJSON(StorageJSON, auth.Metadata) — i.e. the file's top-level
// keys come from the Metadata the plugin returned at parse time. If ParseAuth
// returns only its own 5 keys, every watcher re-parse (triggered by ANY file
// write, including the panel's own PATCH) makes the host persist the file back
// WITHOUT excluded-models / prefix / proxy_url / priority / headers, silently
// undoing the user's model-disable setting within the same second.
//
// Nested credential blocks (auth/account) are excluded — they travel in
// StorageJSON, not Metadata.
func userFieldsFromAuthJSON(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil
	}
	delete(doc, "auth")
	delete(doc, "account")
	if len(doc) == 0 {
		return nil
	}
	return doc
}

// isSafeWorkbuddyAuthPath rejects non-workbuddy filenames, empty paths, and
// traversal attempts. It validates both the basename pattern AND that the path
// does not escape via ".." segments. Callers that need to confine deletes to
// a specific directory should additionally check isPathUnder(path, dir).

func isSafeWorkbuddyAuthPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	// Reject any path containing ".." — prevents traversal regardless of basename.
	if strings.Contains(filepath.ToSlash(path), "../") || strings.Contains(filepath.ToSlash(path), "/..") {
		return false
	}
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	if !strings.HasPrefix(lower, "workbuddy-") && lower != "workbuddy.json" {
		return false
	}
	if !strings.HasSuffix(lower, ".json") {
		return false
	}
	// Path traversal / absolute weirdness: base must equal cleaned base.
	if base != filepath.Base(filepath.Clean(path)) {
		return false
	}
	return true
}

// isPathUnder reports whether path is inside dir (after cleaning both).
// Empty dir means "no constraint" (returns true for any safe path).

func isPathUnder(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return true
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	if cleanPath == cleanDir {
		return false // path is the dir itself, not under it
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}

// deleteAuthFileAt removes a workbuddy auth file. Missing file is success.
// Deprecated: use deleteAuthFileInDir instead (adds directory + absolute path
// confinement). Retained for test coverage of the base safe-delete path.

func deleteAuthFileAt(path string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// deleteAuthFileInDir is like deleteAuthFileAt but additionally requires the
// path to be under dir. Use for lifecycle deletes where the auth directory is
// known — prevents a malicious/buggy host path from deleting arbitrary files.
// The path MUST be absolute (defense against relative-path CWD deletion).

func deleteAuthFileInDir(path, dir string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("refusing to delete relative path: %s", path)
	}
	if dir != "" && !isPathUnder(path, dir) {
		return fmt.Errorf("refusing to delete path outside auth dir: %s (dir=%s)", path, dir)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// hostAuthGetFull returns physical JSON, path, and name for an auth index.
