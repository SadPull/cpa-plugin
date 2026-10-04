package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// version is the plugin release version, set from the entry point via
// SetVersion (ldflags -X main.version on the main package).
var version = "0.0.0-dev"

// SetVersion overrides the reported plugin version.
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		version = v
	}
}

// App implements the plugin RPC surface. All state is behind the atomic
// snapshot in modelVisibility, so intercept calls never take locks.
type App struct {
	visibility *modelVisibility

	hostCallFn func(method string, payload any) (json.RawMessage, error)
	hostMu     sync.Mutex
}

var app = &App{visibility: newModelVisibility()}

// Default returns the process-wide App instance.
func Default() *App { return app }

// SetHostCaller wires the cgo bridge used for host.* callbacks. It is called
// once from cliproxy_plugin_init before any RPC is dispatched.
func (a *App) SetHostCaller(fn func(method string, payload any) (json.RawMessage, error)) {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	a.hostCallFn = fn
}

// hostCall performs a plugin→host callback; best-effort, never panics.
func (a *App) hostCall(method string, payload any) (json.RawMessage, error) {
	a.hostMu.Lock()
	fn := a.hostCallFn
	a.hostMu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("host caller not wired")
	}
	return fn(method, payload)
}

// hostLog is a best-effort host.log callback; failures are dropped.
func hostLog(hostCallbackID, level, message string, fields map[string]any) {
	payload := struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level,omitempty"`
		Message        string         `json:"message,omitempty"`
		Fields         map[string]any `json:"fields,omitempty"`
	}{HostCallbackID: hostCallbackID, Level: level, Message: message, Fields: fields}
	_, _ = app.hostCall(MethodHostLog, payload)
}

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		// Marshaling our own structs cannot fail; fall back anyway.
		return []byte(`{"ok":true,"result":{}}`)
	}
	envelope, err := json.Marshal(Envelope{OK: true, Result: raw})
	if err != nil {
		return []byte(`{"ok":true,"result":{}}`)
	}
	return envelope
}

func errorEnvelope(status int, code, message string) []byte {
	envelope, err := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{
		Code: code, Message: message, HTTPStatus: status,
	}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"marshal failed"}}`)
	}
	return envelope
}

// HandleMethod dispatches one host→plugin RPC and returns the envelope.
// The host permanently unloads plugins that panic, so every failure is
// converted into an error envelope instead.
func (a *App) HandleMethod(method string, request []byte) (response []byte) {
	defer func() {
		if r := recover(); r != nil {
			response = errorEnvelope(500, "plugin_panic", fmt.Sprintf("recovered: %v", r))
		}
	}()

	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		return a.handleLifecycle(request)
	case MethodPluginQuiesce, MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case MethodResponseInterceptAfter:
		return a.handleResponseIntercept(request)
	case MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return errorEnvelope(404, "unknown_method", "method "+method+" is not implemented")
	}
}

func (a *App) handleLifecycle(request []byte) []byte {
	var req LifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope(400, "invalid_request", "decode lifecycle request: "+err.Error())
	}
	if err := a.visibility.configure(req.ConfigYAML); err != nil {
		hostLog("", "error", "configuration rejected: "+err.Error(), nil)
		return errorEnvelope(400, "invalid_config", err.Error())
	}
	snap := a.visibility.snapshot()
	hostLog("", "info", fmt.Sprintf("cpa-model-visibility %s active: %d rule(s), default=%q", version, len(snap.byKey), snap.policy), nil)
	return okEnvelope(registration())
}

func (a *App) handleResponseIntercept(request []byte) []byte {
	var req ResponseInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope(400, "invalid_request", "decode intercept request: "+err.Error())
	}
	resp := ResponseInterceptResponse{}
	if filtered := a.visibility.filterModelList(&req); filtered != nil {
		resp.Body = filtered
	}
	return okEnvelope(resp)
}

func registration() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             "CPA Model Visibility",
			Version:          version,
			Author:           "SadPull",
			GitHubRepository: "https://github.com/SadPull/cpa-plugin",
			Logo:             pluginLogo,
			ConfigFields: []ConfigField{
				{Name: "debug", Type: "boolean", Description: "log each filtered catalog response"},
				{Name: "default", Type: "enum", Description: "what a key without a matching rule sees: empty or full"},
			},
		},
		Capabilities: Capabilities{
			ResponseInterceptor: true,
			ManagementAPI:       true,
		},
	}
}
