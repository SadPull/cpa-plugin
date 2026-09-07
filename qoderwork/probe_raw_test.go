//go:build live

// Raw model-catalog probe: dumps the upstream model/list response exactly as
// the upstream returns it (all scenes, enable flags included), plus endpoint
// and signature variants, to diagnose model-catalog issues.
//
// Run on a host with a real auth file:
//
//	go test -tags live -v -run 'TestProbeRawModelCatalog|TestProbeCatalogVariants|TestProbeSignatureVariants' ./...
//
// Optional: QODER_AUTH_FILE=/path/to/qoder-<uid>.json
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeAuth(t *testing.T) *storedAuth {
	if p := strings.TrimSpace(os.Getenv("QODER_AUTH_FILE")); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read auth file: %v", err)
		}
		sa, err := parseStored(raw)
		if err != nil {
			t.Fatalf("parse auth: %v", err)
		}
		return sa
	}
	matches, _ := filepath.Glob("/root/.cli-proxy-api/qoder-*.json")
	if len(matches) == 0 {
		t.Skip("no /root/.cli-proxy-api/qoder-*.json found — skipping")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	return sa
}

// probeGet performs a GET against url with full COSY signing. signedBody is
// the body string fed into the signature — for a bodyless GET it must be the
// empty string, otherwise the gateway rejects with 403 "Signature invalid".
func probeGet(t *testing.T, sa *storedAuth, url, signedBody string, extraHeaders map[string]string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := applyCosyHeaders(req, sa, signedBody, url, "", false); err != nil {
		t.Fatalf("cosy: %v", err)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestProbeRawModelCatalog dumps the untouched upstream catalog JSON.
func TestProbeRawModelCatalog(t *testing.T) {
	sa := probeAuth(t)
	region := regionForAuth(sa)
	spec := specFor(region)
	t.Logf("auth uid=%s region=%s modelsURL=%s", sa.Account.UID, region, spec.ModelsURL)

	status, raw := probeGet(t, sa, spec.ModelsURL, "", nil)
	t.Logf("HTTP %d, %d bytes", status, len(raw))
	if status != 200 {
		t.Fatalf("body: %.400s", raw)
	}

	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatalf("parse: %v (body=%.400s)", err, raw)
	}
	scenes := make([]string, 0, len(full))
	for scene := range full {
		scenes = append(scenes, scene)
	}
	sortStrings(scenes)
	for _, scene := range scenes {
		var entries []struct {
			Key         string  `json:"key"`
			DisplayName string  `json:"display_name"`
			Enable      bool    `json:"enable"`
			PriceFactor float64 `json:"price_factor"`
		}
		if json.Unmarshal(full[scene], &entries) != nil {
			t.Logf("scene %-12s: (non-list payload) %.120s", scene, full[scene])
			continue
		}
		t.Logf("scene %-12s: %d models", scene, len(entries))
		for _, e := range entries {
			mark := " "
			if !e.Enable {
				mark = "!"
			}
			t.Logf("  %s %-22s %-30s price=%.2f", mark, e.Key, e.DisplayName, e.PriceFactor)
		}
	}
	out := fmt.Sprintf("/tmp/qoder_models_raw_%s.json", time.Now().Format("150405"))
	if err := os.WriteFile(out, raw, 0644); err == nil {
		t.Logf("raw payload saved to %s", out)
	}
}

// TestProbeCatalogVariants tries endpoint variants that may expose a fresher
// catalog (no Encode, openapi host).
func TestProbeCatalogVariants(t *testing.T) {
	sa := probeAuth(t)
	region := regionForAuth(sa)
	spec := specFor(region)

	type variant struct {
		name string
		url  string
	}
	variants := []variant{
		{name: "gateway model/list (baseline)", url: spec.ModelsURL},
		{name: "gateway model/list (no Encode)", url: strings.Replace(spec.ModelsURL, "?Encode=1", "", 1)},
		{name: "openapi algo model/list", url: spec.OpenAPIBase + "/algo/api/v2/model/list?Encode=1"},
	}
	for _, v := range variants {
		status, raw := probeGet(t, sa, v.url, "", nil)
		summary := fmt.Sprintf("HTTP %d, %d bytes", status, len(raw))
		var scenes map[string]json.RawMessage
		chatN := -1
		if json.Unmarshal(raw, &scenes) == nil {
			var chat []struct {
				Key string `json:"key"`
			}
			if c, ok := scenes["chat"]; ok && json.Unmarshal(c, &chat) == nil {
				chatN = len(chat)
			}
		}
		t.Logf("[%s] %s chat_models=%d", v.name, summary, chatN)
	}
}

// TestProbeSignatureVariants pins down the body/signature combination the
// gateway accepts for model/list: the signature covers a body string, so the
// transmitted body must match what was signed. Kept as a regression probe.
func TestProbeSignatureVariants(t *testing.T) {
	sa := probeAuth(t)
	region := regionForAuth(sa)
	spec := specFor(region)
	client := &http.Client{Timeout: 20 * time.Second}

	type variant struct {
		name   string
		method string
		body   []byte // actual transmitted body (nil = no body)
		signed string // body string fed into the signature
	}
	encodedEmpty := qoderEncode([]byte("{}"))
	variants := []variant{
		{name: "GET, no body, signed encodedEmpty (broken)", method: http.MethodGet, signed: encodedEmpty},
		{name: "GET, no body, signed empty string (correct)", method: http.MethodGet, signed: ""},
		{name: "GET, body=encodedEmpty, signed encodedEmpty", method: http.MethodGet, body: []byte(encodedEmpty), signed: encodedEmpty},
	}
	for _, v := range variants {
		req, err := http.NewRequest(v.method, spec.ModelsURL, nil)
		if err != nil {
			continue
		}
		if err := applyCosyHeaders(req, sa, v.signed, spec.ModelsURL, "", false); err != nil {
			t.Fatalf("cosy: %v", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", clientUA)
		if v.body != nil {
			req.Body = io.NopCloser(strings.NewReader(string(v.body)))
			req.ContentLength = int64(len(v.body))
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[%s] ERR %v", v.name, err)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		summary := fmt.Sprintf("HTTP %d, %d bytes", resp.StatusCode, len(raw))
		var scenes map[string]json.RawMessage
		chatN := -1
		if json.Unmarshal(raw, &scenes) == nil {
			var chat []struct {
				Key string `json:"key"`
			}
			if c, ok := scenes["chat"]; ok && json.Unmarshal(c, &chat) == nil {
				chatN = len(chat)
			}
		}
		t.Logf("[%s] %s chat_models=%d", v.name, summary, chatN)
	}
}

// TestProbeCNToolCalls verifies the CN gateway honours client-provided tools
// and a replaced system prompt, returning structured tool_calls deltas.
func TestProbeCNToolCalls(t *testing.T) {
	sa := probeAuth(t)
	region := regionForAuth(sa)
	spec := specFor(region)

	// Build the template body, then override system + tools the way the fix
	// will do inside buildQoderBody.
	qwReq := &openAIRequest{
		Model: "qwen3.7-flash",
		Messages: []openAIMessage{
			{Role: "system", Content: "You are a helpful server assistant. You MUST use the run_command tool for any shell task. Available tool: run_command(command) - runs a shell command."},
			{Role: "user", Content: "Check the server load with the run_command tool. Call the tool now."},
		},
	}
	body, err := buildQoderBody(qwReq, "q37fmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(body, &base); err != nil {
		t.Fatal(err)
	}
	base["tools"] = []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "run_command",
			"description": "Run a shell command on the server and return its output.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string", "description": "the shell command to run"}},
				"required":   []string{"command"},
			},
		},
	}}
	body, _ = json.Marshal(base)

	encoded := qoderEncode(body)
	req, err := http.NewRequest(http.MethodPost, spec.ChatURL, strings.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := applyCosyHeaders(req, sa, encoded, spec.ChatURL, "q37fmodel", true); err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", clientUA)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("HTTP %d, %d bytes", resp.StatusCode, len(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("body: %.300s", raw)
	}
	var content strings.Builder
	var finish string
	toolCallLines := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		var outer map[string]any
		if json.Unmarshal([]byte(payload), &outer) != nil {
			continue
		}
		inner, ok := outer["body"].(string)
		if !ok || inner == "[DONE]" {
			continue
		}
		if strings.Contains(inner, "tool_calls") || strings.Contains(inner, "run_command") {
			toolCallLines++
			if toolCallLines <= 3 {
				t.Logf("tool line: %.300s", inner)
			}
		}
		var chunk struct {
			Choices []struct {
				Delta  map[string]any `json:"delta"`
				Finish string         `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(inner), &chunk) == nil {
			for _, c := range chunk.Choices {
				if c.Finish != "" {
					finish = c.Finish
				}
				if tc, ok := c.Delta["tool_calls"]; ok {
					content.WriteString(fmt.Sprintf("[TOOL_CALLS %v] ", tc))
				}
				if ct, ok := c.Delta["content"].(string); ok {
					content.WriteString(ct)
				}
			}
		}
	}
	t.Logf("finish_reason=%q tool_lines=%d aggregated=%.500s", finish, toolCallLines, content.String())
}

// TestProbeCNToolResultShape finds the message shape the CN gateway accepts
// for feeding tool results back (role:"tool" appears to be dropped, making
// the model re-call the tool forever).
func TestProbeCNToolResultShape(t *testing.T) {
	sa := probeAuth(t)
	spec := specFor(RegionCN)
	client := &http.Client{Timeout: 120 * time.Second}

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "bash",
			"description": "Run a shell command",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
		},
	}}
	system := "You are DSH. Use the bash tool for shell tasks. Call it at most once per turn."

	type variant struct {
		name    string
		assistant map[string]any
		result    map[string]any
	}
	variants := []variant{
		{
			name:      "role=tool + tool_call_id + assistant tool_calls",
			assistant: map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"uptime"}`}}}},
			result:    map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "load average: 0.26, 0.21, 0.09"},
		},
		{
			name:      "role=tool plain (no ids)",
			assistant: map[string]any{"role": "assistant", "content": "Calling bash.", "tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"uptime"}`}}}},
			result:    map[string]any{"role": "tool", "content": "load average: 0.26, 0.21, 0.09"},
		},
		{
			name:      "tool result as user text",
			assistant: map[string]any{"role": "assistant", "content": "Calling bash."},
			result:    map[string]any{"role": "user", "content": "Tool result (bash): load average: 0.26, 0.21, 0.09"},
		},
		{
			name:      "assistant tool_calls + result as user text",
			assistant: map[string]any{"role": "assistant", "content": "Calling bash.", "tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"command":"uptime"}`}}}},
			result:    map[string]any{"role": "user", "content": "Tool result (bash) [call_1]: load average: 0.26, 0.21, 0.09"},
		},
	}
	for _, v := range variants {
		messages := []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": "Check the server load with the bash tool. Call it at most once."},
			v.assistant,
			v.result,
		}
		body, _ := json.Marshal(map[string]any{
			"model": "qwen3.7-flash", "messages": messages, "stream": true,
			"stream_options": map[string]any{"include_usage": true},
			"metadata":       map[string]any{"context": map[string]any{"request_id": "probe", "request_set_id": "probe", "session_id": "probe", "task_id": "common", "client_type": "qodercli"}},
			"tools":          tools,
		})
		encoded := qoderEncode(body)
		req, err := http.NewRequest(http.MethodPost, spec.ChatURL, strings.NewReader(encoded))
		if err != nil {
			continue
		}
		if err := applyCosyHeaders(req, sa, encoded, spec.ChatURL, "q37fmodel", true); err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", clientUA)
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("[%s] ERR %v", v.name, err)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var content strings.Builder
		var finish string
		var sawToolCall bool
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimPrefix(line, "data:")
			var outer map[string]any
			if json.Unmarshal([]byte(payload), &outer) != nil {
				continue
			}
			inner, ok := outer["body"].(string)
			if !ok || inner == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta  map[string]any `json:"delta"`
					Finish string         `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(inner), &chunk) == nil {
				for _, c := range chunk.Choices {
					if c.Finish != "" {
						finish = c.Finish
					}
					if tc, ok := c.Delta["tool_calls"]; ok {
						sawToolCall = true
						content.WriteString(fmt.Sprintf("[TOOL %v] ", tc))
					}
					if ct, ok := c.Delta["content"].(string); ok {
						content.WriteString(ct)
					}
				}
			}
		}
		t.Logf("[%s] finish=%q toolCall=%v content=%.150s", v.name, finish, sawToolCall, content.String())
	}
}
