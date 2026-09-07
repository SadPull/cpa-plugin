// body.go constructs the Qoder agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max", "qmodel_38max":
		return "qmodel_38max"
	case "qwen3.8-flash", "qfmodel":
		return "qfmodel"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.7-flash", "q37fmodel":
		return "q37fmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "qwen3.8-max-preview", "qmodel_preview":
		return "qmodel_preview" // retired upstream — kept for old clients
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.3", "gmodel":
		return "gmodel"
	case "glm-5.3-flash", "gfmodel":
		return "gfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// flexibleContent accepts every content shape OpenAI-style clients send:
// a plain string, an array of typed parts (text / image_url / input_image /
// ...), an object, or null. Arrays are flattened to the text form the CN
// gateway consumes (text parts joined with blank lines, images noted as
// "[image] <url>" placeholders) — the same normalization QoderGateway uses.
// Unknown shapes degrade to their raw JSON text instead of failing the whole
// payload parse (a strict string here 503'd every multi-part request).
type flexibleContent string

func (f *flexibleContent) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexibleContent(s)
		return nil
	}
	var parts []openAIContentPart
	if err := json.Unmarshal(data, &parts); err == nil {
		*f = flexibleContent(flattenContentParts(parts))
		return nil
	}
	// Object / number / bool / anything else: keep the request alive by
	// embedding the raw JSON text.
	*f = flexibleContent(trimmed)
	return nil
}

// openAIContentPart is one entry of an array-style content field.
type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"` // {"url":...} or a bare string
}

func flattenContentParts(parts []openAIContentPart) string {
	var texts []string
	for _, p := range parts {
		switch strings.ToLower(p.Type) {
		case "text":
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		case "image_url", "input_image":
			if url := imageURLOf(p.ImageURL); url != "" {
				texts = append(texts, "[image] "+url)
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

// imageURLOf extracts the URL from an image part payload, tolerating both
// {"url": "..."} and a bare "..." string form.
func imageURLOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.URL != "" {
		return obj.URL
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// openAIMessage is one message in the OpenAI chat completion format.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    flexibleContent  `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIToolCall mirrors one entry of an assistant message's tool_calls.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model      string            `json:"model"`
	Messages   []openAIMessage   `json:"messages"`
	Stream     bool              `json:"stream"`
	Tools      []json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage   `json:"tool_choice,omitempty"`
}

// extractLatestUserPrompt returns the content of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return string(messages[i].Content)
		}
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// Tools: pass the client's tool schemas straight through. Without this the
	// model only sees the baseprompt's built-in Qoder CLI tools and invents
	// calls for tools the client does not have (observed: "Bash" -> unknown
	// tool on agent harnesses).
	if len(req.Tools) > 0 {
		base["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		base["tool_choice"] = req.ToolChoice
	}

	// messages: when the client supplies its own system prompt, it REPLACES the
	// template's — the baseprompt system conditions the model into the Qoder
	// CLI persona (its own tool set), which is exactly what breaks client-side
	// tool routing. With no client system, keep the template's.
	var systemMsgs []any
	clientSystem := false
	for _, m := range req.Messages {
		if m.Role == "system" && strings.TrimSpace(string(m.Content)) != "" {
			clientSystem = true
			break
		}
	}
	if msgs, ok := base["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "system" && !clientSystem {
					systemMsgs = append(systemMsgs, m)
				}
			}
		}
	}
	if clientSystem {
		for _, m := range req.Messages {
			if m.Role == "system" {
				systemMsgs = append(systemMsgs, map[string]any{"role": "system", "content": string(m.Content)})
			}
		}
	}
	// Append the actual conversation. Assistant tool_calls and tool results are
	// passed through in OpenAI form — the gateway emits OpenAI-style tool_calls
	// deltas itself, so it understands the same shapes back.
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue // handled above
		}
		entry := map[string]any{"role": m.Role, "content": string(m.Content)}
		if len(m.ToolCalls) > 0 {
			entry["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		systemMsgs = append(systemMsgs, entry)
	}
	base["messages"] = systemMsgs

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}
