package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseOpenAIRequest mirrors the CN executor's payload parse — the exact path
// that 503'd on array content before flexibleContent existed.
func parseOpenAIRequest(t *testing.T, payload string) *openAIRequest {
	t.Helper()
	req := &openAIRequest{}
	if err := json.Unmarshal([]byte(payload), req); err != nil {
		t.Fatalf("payload parse: %v", err)
	}
	return req
}

func TestFlexibleContentShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "plain string",
			payload: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want:    "hi",
		},
		{
			name: "array of text parts",
			payload: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}`,
			want: "hello \n\nworld",
		},
		{
			name: "array with object-style image",
			payload: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
			want: "look\n\n[image] https://x/y.png",
		},
		{
			name: "array with bare-string image",
			payload: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"image_url","image_url":"data:image/png;base64,AAA"}]}]}`,
			want: "[image] data:image/png;base64,AAA",
		},
		{
			name:    "null content",
			payload: `{"model":"m","messages":[{"role":"assistant","content":null}]}`,
			want:    "",
		},
		{
			name:    "object content degrades to raw JSON",
			payload: `{"model":"m","messages":[{"role":"user","content":{"text":"odd"}}]}`,
			want:    `{"text":"odd"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := parseOpenAIRequest(t, tc.payload)
			if len(req.Messages) != 1 {
				t.Fatalf("messages: %d", len(req.Messages))
			}
			if got := string(req.Messages[0].Content); got != tc.want {
				t.Fatalf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCNBodyAcceptsArrayContent reproduces the reported 503: a multi-part
// user message routed to the CN executor must build a body, not fail.
func TestCNBodyAcceptsArrayContent(t *testing.T) {
	payload := `{"model":"qwen3.8-max","messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":[
			{"type":"text","text":"What is in this picture?"},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`
	req := parseOpenAIRequest(t, payload)
	if got := extractLatestUserPrompt(req.Messages); !strings.Contains(got, "What is in this picture?") || !strings.Contains(got, "example.com/cat.png") {
		t.Fatalf("prompt flatten: %q", got)
	}
	body, err := buildQoderBody(req, "qmodel_38max", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["model_config"] == nil {
		t.Fatal("model_config missing")
	}
}

// TestCNBodyPassesThroughToolsAndSystem covers the tool-calling fix: client
// tools reach the upstream body, a client system prompt replaces the Qoder CLI
// baseprompt (whose built-in "Bash" tool poisoned tool naming), and assistant
// tool_calls / tool results round-trip in OpenAI form.
func TestCNBodyPassesThroughToolsAndSystem(t *testing.T) {
	payload := `{"model":"qwen3.7-flash","messages":[
		{"role":"system","content":"You are DSH. Use the bash tool for shell tasks."},
		{"role":"user","content":"check load"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"uptime\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"load average: 0.26"}],
		"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],
		"tool_choice":"auto"}`
	req := parseOpenAIRequest(t, payload)
	body, err := buildQoderBody(req, "q37fmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	// tools injected verbatim
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: %v", m["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Fatalf("tool name: %v", fn["name"])
	}
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice: %v", m["tool_choice"])
	}
	// client system replaces the baseprompt system
	msgs := m["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are DSH. Use the bash tool for shell tasks." {
		t.Fatalf("system replacement: %v", sys)
	}
	for _, mi := range msgs {
		mm := mi.(map[string]any)
		if c, _ := mm["content"].(string); strings.Contains(c, "Qoder, an interactive CLI tool") {
			t.Fatalf("baseprompt system leaked into messages")
		}
	}
	// assistant tool_calls + tool result round-trip
	var sawToolCalls, sawToolResult bool
	for _, mi := range msgs {
		mm := mi.(map[string]any)
		if mm["role"] == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) == 1 {
				call := tcs[0].(map[string]any)
				if call["id"] == "call_1" {
					sawToolCalls = true
				}
			}
		}
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_1" {
			sawToolResult = true
		}
	}
	if !sawToolCalls || !sawToolResult {
		t.Fatalf("tool_calls=%v tool_result=%v", sawToolCalls, sawToolResult)
	}
}

// TestCNBodyKeepsTemplateSystemWithoutClientSystem: no client system → the
// baseprompt system stays (it is required for normal template behaviour).
func TestCNBodyKeepsTemplateSystemWithoutClientSystem(t *testing.T) {
	payload := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := parseOpenAIRequest(t, payload)
	body, err := buildQoderBody(req, "lite", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first message: %v", first)
	}
	if c, _ := first["content"].(string); !strings.Contains(c, "Qoder") {
		t.Fatalf("template system missing: %.80s", c)
	}
	// Without client tools the template's built-in Qoder CLI tool set stays
	// (it includes a "Bash" tool — the source of the hallucinated "Bash"
	// calls on agent harnesses; the client-tools override in
	// TestCNBodyPassesThroughToolsAndSystem is what fixes that).
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("template builtin tools expected, got %v", m["tools"])
	}
}
