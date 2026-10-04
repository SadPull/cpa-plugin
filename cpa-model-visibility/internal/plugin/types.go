// Package plugin implements the cpa-model-visibility CLIProxyAPI plugin:
// per-API-key filtering of the /v1/models catalog served by the host.
//
// Protocol contract (verified against router-for-me/CLIProxyAPI v8.0.11,
// commit e2bff010):
//   - C ABI: only cliproxy_plugin_init is looked up by name; the plugin fills
//     cliproxy_plugin_api {abi_version, call, free_buffer, shutdown}.
//   - RPC: JSON envelope {"ok":bool,"result":...,"error":{code,message,http_status}}.
//   - plugin.register request: {"config_yaml":"<base64>","schema_version":N};
//     response result {"schema_version","metadata","capabilities"} with the
//     plugin schema_version capped at the host schema (6).
//   - response.intercept_after request: pluginapi.ResponseInterceptRequest with
//     Go field names (no json tags on the host side) + "host_callback_id".
//     Response result {"Headers","Body","ClearHeaders"}; a non-empty Body
//     replaces the response body, an empty Body leaves it untouched
//     (handlers_interceptors.go: `if len(resp.Body) > 0`).
//   - management.register response result {"routes":[{"Method","Path",...}]}.
package plugin

import "encoding/json"

const (
	// PluginID is the plugin identity: the .so file name, the
	// plugins.configs key and the management route segment must match it.
	PluginID = "cpa-model-visibility"
	// ABIVersion is the C ABI version (pluginabi.ABIVersion).
	ABIVersion uint32 = 1
	// SchemaVersion is the RPC JSON contract level the plugin implements.
	// 6 keeps management JSON responses free of legacy HTML escaping.
	SchemaVersion uint32 = 6
)

// Host RPC method names (pluginabi.Method* values, duplicated to stay
// dependency-free from the CPA Go module).
const (
	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"
	MethodPluginQuiesce     = "plugin.quiesce"
	MethodPluginShutdown    = "plugin.shutdown"

	MethodResponseInterceptAfter = "response.intercept_after"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	MethodHostLog = "host.log"
)

// Envelope is the RPC response frame sent back to the host.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a failure back to the host.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// Metadata mirrors pluginapi.Metadata (no json tags on the host side, so the
// Go field names are the wire keys).
type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []ConfigField
}

// ConfigField mirrors pluginapi.ConfigField.
type ConfigField struct {
	Name        string
	Type        string
	Description string
}

// Capabilities is the wire subset of pluginapi.Capabilities
// (internal/pluginhost/rpc_schema.go rpcCapabilities JSON keys).
type Capabilities struct {
	ResponseInterceptor bool `json:"response_interceptor"`
	ManagementAPI       bool `json:"management_api"`
}

// Registration is the plugin.register result.
type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// LifecycleRequest is the plugin.register / plugin.reconfigure request.
type LifecycleRequest struct {
	// ConfigYAML is the base64-encoded plugins.configs.<PluginID> subtree.
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// ResponseInterceptRequest mirrors pluginapi.ResponseInterceptRequest.
// Field names are the wire keys; the host sends no json tags.
type ResponseInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	RequestHeaders  map[string][]string
	ResponseHeaders map[string][]string
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any

	// HostCallbackID is injected by the host RPC wrapper.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ResponseInterceptResponse mirrors pluginapi.ResponseInterceptResponse.
// An empty (or omitted) Body leaves the response body untouched.
type ResponseInterceptResponse struct {
	Headers      map[string][]string `json:"Headers,omitempty"`
	Body         []byte              `json:"Body,omitempty"`
	ClearHeaders []string            `json:"ClearHeaders,omitempty"`
}

// ManagementRequest mirrors pluginapi.ManagementRequest.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers map[string][]string
	Query   map[string][]string
	Body    []byte

	// HostCallbackID is injected by the host RPC wrapper.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ManagementResponse mirrors pluginapi.ManagementResponse.
type ManagementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// ManagementRoute mirrors pluginapi.ManagementRoute (wire subset).
type ManagementRoute struct {
	Method      string
	Path        string
	Description string
}
