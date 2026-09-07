// chat_global.go implements the global-realm (qoder.sh) inference data path —
// the Qoder CLI's current protocol: POST {model}/v1/chat/completions is
// OpenAI-native end to end. Auth is a plain Bearer (dt-/jt-), the request body
// is the client's chat-completions payload with metadata.context attached, and
// the response is standard OpenAI SSE (no COSY signing, no QoderEncoding, no
// nested-SSE unwrapping). Verified live 2026-08 (QoderGateway protocol
// research §2/§7). The CN counterpart is chat_cn.go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// qoderCLIUA is the User-Agent the api2-v2 inference endpoint expects.
const qoderCLIUA = "qoder/1.1.16"

// globalModelKey normalizes a CPA-facing model name into the upstream model id
// for the global endpoint: dynamic catalog slug → legacy translation table →
// passthrough; empty falls back to "lite" (the Qoder CLI default, verified
// against api2-v2).
func globalModelKey(model string) string {
	key := resolveUpstreamModelKey(model, RegionGlobal)
	if key == "" {
		return "lite"
	}
	return key
}

// buildGlobalBody renders the OpenAI-native upstream body. Everything the
// client sent (messages with multi-part content, tool_calls, tools, stop,
// max_tokens, temperature, reasoning_effort, …) passes through untouched;
// only model / stream / stream_options / metadata.context are (re)set.
func buildGlobalBody(rawPayload []byte, model string) ([]byte, error) {
	var base map[string]any
	if len(rawPayload) > 0 {
		if err := json.Unmarshal(rawPayload, &base); err != nil {
			return nil, fmt.Errorf("payload parse: %w", err)
		}
	} else {
		base = map[string]any{}
	}
	if _, ok := base["messages"].([]any); !ok {
		base["messages"] = []any{}
	}
	base["model"] = model
	// Defensive default: callers resolve the model via globalModelKey, but an
	// empty id must never reach upstream — "lite" is the verified CLI default.
	if m, _ := base["model"].(string); strings.TrimSpace(m) == "" {
		base["model"] = "lite"
	}
	// Upstream answers with SSE only; the sync executor folds chunks into a
	// completion, so streaming is always requested.
	base["stream"] = true
	base["stream_options"] = map[string]any{"include_usage": true}
	rid := uuid.NewString()
	base["metadata"] = map[string]any{
		"context": map[string]any{
			"request_id":     rid,
			"request_set_id": rid,
			"session_id":     uuid.NewString(),
			"task_id":        "common",
			"client_type":    "qodercli",
		},
	}
	return json.Marshal(base)
}

// applyGlobalHeaders sets the Bearer + CLI identity headers for one global
// inference request.
func applyGlobalHeaders(req *http.Request, sa *storedAuth, sessionID string, sse bool) {
	rid := uuid.NewString()
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	if sse {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", qoderCLIUA)
	req.Header.Set("X-Request-ID", rid)
	req.Header.Set("X-Session-ID", sessionID)
}

// globalSessionID derives the X-Session-ID / metadata.context.session_id pair
// so both headers carry the same UUID (the CLI keeps them aligned).
func globalSessionID() string { return uuid.NewString() }

// execExecuteGlobal runs a synchronous chat completion: stream upstream, fold
// the standard OpenAI SSE into one chat.completion object (aggregateCompletion
// consumes standard SSE directly — no nested unwrapping on this realm).
func execExecuteGlobal(req pluginapi.ExecutorRequest, sa *storedAuth) ([]byte, error) {
	upstreamModel := globalModelKey(stripProviderPrefix(req.Model))
	started := time.Now()
	authUID := sa.Account.UID

	sessionID := globalSessionID()
	body, err := buildGlobalBody(req.Payload, upstreamModel)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}
	chatURL := specFor(RegionGlobal).ChatURL
	httpReq, err := http.NewRequest(http.MethodPost, chatURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	applyGlobalHeaders(httpReq, sa, sessionID, true)

	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		payload, _ := io.ReadAll(reader)
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
		reconcileAfterExecutorError(req.AuthID, statusCode, string(payload))
		return nil, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 200))
	}
	completion, err := aggregateCompletion(reader, req.Model)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// execStreamGlobal streams a global-realm completion: synchronous chunk
// collection when the host passed no async stream id, otherwise a background
// pump emitting each standard OpenAI chunk via host.stream.emit.
func execStreamGlobal(req executorStreamRequest, sa *storedAuth) ([]byte, error) {
	upstreamModel := globalModelKey(stripProviderPrefix(req.Model))
	started := time.Now()
	authUID := sa.Account.UID

	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}
	sessionID := globalSessionID()
	body, err := buildGlobalBody(bodyRaw, upstreamModel)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)
	chatURL := specFor(RegionGlobal).ChatURL

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		httpReq, err := http.NewRequest(http.MethodPost, chatURL, strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		applyGlobalHeaders(httpReq, sa, sessionID, true)
		chunks, statusCode, errCollect := collectUpstreamStream(httpReq, sseFramed, collector, unwrapOpenAIChunk)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errCollect.Error())
			return nil, errCollect
		}
		publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), false, 0, "")
		invalidateAccountCredits(req.AuthID, authUID)
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately; the pump emits chunks via host.stream.emit.
	ctx, cancel := context.WithCancel(context.Background())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, strings.NewReader(string(body)))
	if err != nil {
		cancel()
		streamEmitError(req.StreamID, err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	applyGlobalHeaders(httpReq, sa, sessionID, true)
	go pumpUpstreamStream(httpReq, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, unwrapOpenAIChunk)
	return okEnvelope(streamResponse{Headers: headers})
}

// unwrapOpenAIChunk passes one standard OpenAI SSE data line through: the
// global realm needs no envelope unwrapping. done=true on the [DONE] sentinel.
func unwrapOpenAIChunk(payload []byte) (string, bool, bool) {
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return "", false, false
	}
	if s == "[DONE]" {
		return "", false, true
	}
	var probe map[string]any
	if json.Unmarshal([]byte(s), &probe) != nil {
		return "", false, false
	}
	return s, true, false
}
