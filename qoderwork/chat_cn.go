// chat_cn.go implements the CN-realm (qoder.com.cn) inference data path:
// COSY-signed, QoderEncoding-encoded requests against the legacy gateway
// agent_chat_generation endpoint, with the double-nested SSE response shape.
// Protocol details live in sign.go / encoding.go / body.go. The global realm
// counterpart is chat_global.go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// execExecuteCN runs a synchronous chat completion through the COSY gateway:
// build the template body, QoderEncoding-encode it, sign, stream, and fold the
// nested SSE into one chat.completion object.
func execExecuteCN(req pluginapi.ExecutorRequest, sa *storedAuth) ([]byte, error) {
	upstreamModel := resolveUpstreamModelKey(req.Model, regionForAuth(sa))
	started := time.Now()
	authUID := sa.Account.UID
	// Build the agent_chat_generation body from the OpenAI request, then
	// QoderEncoding-encode it. The template embeds a ~10k-token system prompt
	// that the server requires for normal behaviour (KNOWLEDGE §5.2).
	qwReq := &openAIRequest{}
	if err := json.Unmarshal(req.Payload, qwReq); err != nil && len(req.Payload) > 0 {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "payload parse: "+err.Error())
		return nil, fmt.Errorf("payload parse: %w", err)
	}
	body, err := buildQoderBody(qwReq, upstreamModel, uiUserType(nil))
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}
	encodedBody := qoderEncode(body)
	chatURL := specFor(RegionCN).ChatURL
	httpReq, err := http.NewRequest(http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, chatURL, upstreamModel, true); err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "cosy: "+err.Error())
		return nil, fmt.Errorf("cosy: %w", err)
	}
	// Compliance: route via host.http.do_stream so request-log captures the
	// outbound call. Read entire body via the bridge, then fold SSE → completion.
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
	completion, err := aggregateQoderSSE(reader, req.Model)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// execStreamCN streams a CN-realm completion: synchronous chunk collection
// when the host passed no async stream id, otherwise a background pump that
// emits each unwrapped inner OpenAI chunk via host.stream.emit.
func execStreamCN(req executorStreamRequest, sa *storedAuth) ([]byte, error) {
	upstreamModel := resolveUpstreamModelKey(req.Model, regionForAuth(sa))
	started := time.Now()
	authUID := sa.Account.UID

	// Build the gateway body (template-based) and QoderEncoding-encode.
	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}
	qwReq := &openAIRequest{}
	if err := json.Unmarshal(bodyRaw, qwReq); err != nil && len(bodyRaw) > 0 {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "payload parse: "+err.Error())
		return nil, fmt.Errorf("payload parse: %w", err)
	}
	body, err := buildQoderBody(qwReq, upstreamModel, uiUserType(nil))
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}
	encodedBody := qoderEncode(body)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		chunks, statusCode, errCollect := collectUpstreamStreamCN(encodedBody, sa, upstreamModel, sseFramed, collector)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errCollect.Error())
			return nil, errCollect
		}
		publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), false, 0, "")
		invalidateAccountCredits(req.AuthID, authUID)
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	// Use context.Background() (not nil) so the request can be cancelled when the
	// client disconnects — otherwise the pump keeps reading a dead upstream until
	// sharedHTTPClient's 120s timeout, holding a pool slot the whole time.
	ctx, cancel := context.WithCancel(context.Background())
	chatURL := specFor(RegionCN).ChatURL
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		cancel()
		streamEmitError(req.StreamID, err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, chatURL, upstreamModel, true); err != nil {
		cancel()
		streamEmitError(req.StreamID, "cosy: "+err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	go pumpUpstreamStreamCN(httpReq, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID)
	return okEnvelope(streamResponse{Headers: headers})
}

// pumpUpstreamStreamCN reads the CN gateway's nested SSE in the background and
// emits each unwrapped inner OpenAI chunk to the host stream (see stream.go
// pumpUpstreamStream for the shared contract).
func pumpUpstreamStreamCN(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID string) {
	pumpUpstreamStream(httpReq, cancel, streamID, sseFramed, requestedModel, upstreamModel, authUID, started, authID, unwrapCNBody)
}

// collectUpstreamStreamCN is the synchronous CN collection path (renamed from
// collectUpstreamStreamQoder for region symmetry with chat_global.go).
func collectUpstreamStreamCN(encodedBody string, sa *storedAuth, modelKey string, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	chatURL := specFor(RegionCN).ChatURL
	httpReq, err := http.NewRequest(http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		return nil, 0, err
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, chatURL, modelKey, true); err != nil {
		return nil, 0, fmt.Errorf("cosy: %w", err)
	}
	return collectUpstreamStream(httpReq, sseFramed, collector, unwrapCNBody)
}

// unwrapCNBody unwraps one CN gateway SSE data line into the inner OpenAI
// chunk JSON (double-nested: outer {"body":"<json-string>"}). ok=false for
// non-data lines; done=true on the terminal [DONE] sentinel.
func unwrapCNBody(payload []byte) (string, bool, bool) {
	var outer map[string]any
	if json.Unmarshal(payload, &outer) != nil {
		return "", false, false
	}
	bodyStr, isStr := outer["body"].(string)
	if !isStr {
		return "", false, false
	}
	if bodyStr == "[DONE]" {
		return "", false, true
	}
	return bodyStr, true, false
}
