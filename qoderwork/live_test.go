//go:build live

// Live protocol verification against real Qoder endpoints. Run with a real
// credential (dt- device token or jt- job token from a global-realm account):
//
//	QODER_LIVE_TOKEN=dt-xxxx go test -tags live -v -run TestLiveGlobal ./...
//
// Optional QODER_LIVE_MODEL (default "lite"). This test NEVER refreshes or
// rotates the token — it is read-only against the account.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func liveToken(t *testing.T) string {
	tok := strings.TrimSpace(os.Getenv("QODER_LIVE_TOKEN"))
	if tok == "" {
		t.Skip("QODER_LIVE_TOKEN not set — skipping live test")
	}
	return tok
}

// TestLiveGlobalUserinfo checks GET openapi.qoder.sh/api/v1/userinfo.
func TestLiveGlobalUserinfo(t *testing.T) {
	tok := liveToken(t)
	req, _ := http.NewRequest(http.MethodGet, specFor(RegionGlobal).OpenAPIBase+"/api/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("userinfo HTTP %d: %s", resp.StatusCode, raw)
	}
	var ui struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &ui)
	t.Logf("userinfo OK: id=%s name=%s", ui.ID, ui.Name)
}

// TestLiveGlobalChat runs one streaming chat completion and prints chunks.
func TestLiveGlobalChat(t *testing.T) {
	tok := liveToken(t)
	model := strings.TrimSpace(os.Getenv("QODER_LIVE_MODEL"))
	if model == "" {
		model = "lite"
	}
	rid := uuid.NewString()
	sid := uuid.NewString()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "Reply with exactly: OK"}},
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
		"metadata": map[string]any{
			"context": map[string]any{
				"request_id":     rid,
				"request_set_id": rid,
				"session_id":     sid,
				"task_id":        "common",
				"client_type":    "qodercli",
			},
		},
	}
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, specFor(RegionGlobal).ChatURL, strings.NewReader(string(payload)))
	applyGlobalHeaders(req, &storedAuth{Auth: storedTokens{AccessToken: tok}}, sid, true)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("chat HTTP %d: %s", resp.StatusCode, raw)
	}
	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	chunks := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		inner, ok, done := unwrapOpenAIChunk([]byte(payload))
		if done || !ok {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(inner), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
			chunks++
		}
		if chunk.Usage != nil {
			t.Logf("usage: %v", chunk.Usage)
		}
	}
	t.Logf("chat OK: %d chunks, model=%s, content=%q", chunks, model, content.String())
	if chunks == 0 {
		t.Fatal("no chunks received")
	}
}

// TestLiveGlobalQuota checks GET openapi.qoder.sh/api/v2/quota/usage.
func TestLiveGlobalQuota(t *testing.T) {
	tok := liveToken(t)
	req, _ := http.NewRequest(http.MethodGet, specFor(RegionGlobal).OpenAPIBase+"/api/v2/quota/usage", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("quota HTTP %d: %s", resp.StatusCode, raw)
	}
	t.Logf("quota OK: %s", fmt.Sprintf("%.160s", raw))
}
