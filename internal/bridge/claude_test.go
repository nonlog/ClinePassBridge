package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decodeClaudeEventForTest(t *testing.T, payload []byte) (string, map[string]any) {
	t.Helper()
	text := string(payload)
	if strings.Count(text, "event: ") != 1 {
		t.Fatalf("Claude chunk must contain exactly one SSE event, got %q", text)
	}
	var event string
	var data []byte
	for _, line := range strings.Split(text, string([]byte{10})) {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	if event == "" || len(data) == 0 {
		t.Fatalf("invalid Claude SSE event: %q", text)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("invalid Claude SSE JSON for %s: %v", event, err)
	}
	return event, body
}

func TestRegistrationDeclaresNativeClaudeOutput(t *testing.T) {
	reg := registration().(map[string]any)
	capabilities := reg["capabilities"].(map[string]any)
	formats := capabilities["executor_output_formats"].([]string)
	hasChat, hasClaude := false, false
	for _, format := range formats {
		switch format {
		case "chat-completions":
			hasChat = true
		case "claude":
			hasClaude = true
		}
	}
	if !hasChat || !hasClaude {
		t.Fatalf("executor output formats = %#v, want chat-completions and claude", formats)
	}
}

func TestNativeClaudeNonstreamPreservesReasoningToolsNamesAndUsage(t *testing.T) {
	s := registeredService(t, "native")
	data := map[string]any{
		"id": "chatcmpl-test",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "done",
				"reasoning_content": "thinking",
				"tool_calls": []any{map[string]any{
					"id": "call:1",
					"function": map[string]any{
						"name":      "lookuptool",
						"arguments": "{"q":"x"}",
					},
				}},
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     20,
			"completion_tokens": 4,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 12,
			},
		},
		"provider": "deepseek",
	}
	h := newFakeHost(jsonPlan(map[string]any{"success": true, "data": data}))
	s.SetHost(h.call)

	var req ExecutorRequest
	if err := json.Unmarshal(executorRequest(""), &req); err != nil {
		t.Fatal(err)
	}
	req.Format = "claude"
	req.SourceFormat = "chat-completions"
	req.OriginalRequest = jsonBytes(map[string]any{
		"model":  "deepseek-flash",
		"stream": false,
		"tools": []any{map[string]any{
			"name": "LookupTool",
		}},
	})

	result, err := s.Handle("executor.execute", jsonBytes(req))
	if err != nil {
		t.Fatalf("native Claude nonstream: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.(Response).Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != "deepseek-flash" {
		t.Fatalf("Claude message envelope = %#v", body)
	}
	content := list(body["content"])
	if len(content) != 3 {
		t.Fatalf("Claude content blocks = %#v", content)
	}
	if object(content[0])["type"] != "thinking" || object(content[0])["thinking"] != "thinking" {
		t.Fatalf("thinking block = %#v", content[0])
	}
	if object(content[1])["type"] != "text" || object(content[1])["text"] != "done" {
		t.Fatalf("text block = %#v", content[1])
	}
	tool := object(content[2])
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "LookupTool" {
		t.Fatalf("tool block = %#v", tool)
	}
	if object(tool["input"])["q"] != "x" || body["stop_reason"] != "tool_use" {
		t.Fatalf("tool input/stop reason = %#v / %#v", tool["input"], body["stop_reason"])
	}
	usage := object(body["usage"])
	if number(usage["input_tokens"]) != 8 || number(usage["output_tokens"]) != 4 || number(usage["cache_read_input_tokens"]) != 12 {
		t.Fatalf("Claude usage = %#v", usage)
	}
	if len(s.logs) != 1 || s.logs[0].OutputFormat != "claude" || s.logs[0].Provider != "deepseek" {
		t.Fatalf("request log = %#v", s.logs)
	}
}

func TestNativeClaudeStreamingProducesCompleteUnbatchedEvents(t *testing.T) {
	s := registeredService(t, "native-fallback")
	reasoning := sseFrame(map[string]any{
		"id": "chatcmpl-stream",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"reasoning_content": "think"},
		}},
	})
	text := sseFrame(map[string]any{
		"id": "chatcmpl-stream",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": "hello"},
		}},
	})
	toolStart := sseFrame(map[string]any{
		"id": "chatcmpl-stream",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0,
				"id":    "call:1",
				"function": map[string]any{
					"name":      "lookuptool",
					"arguments": "{"q":",
				},
			}}},
		}},
	})
	toolEnd := sseFrame(map[string]any{
		"id": "chatcmpl-stream",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0,
				"function": map[string]any{
					"arguments": ""x"}",
				},
			}}},
		}},
	})
	finish := sseFrame(map[string]any{
		"id": "chatcmpl-stream",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens":     20,
			"completion_tokens": 7,
			"prompt_tokens_details": map[string]any{
				"cached_tokens":        8,
				"cache_write_tokens":   2,
			},
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": 2,
			},
		},
	})
	h := newFakeHost(ssePlan(reasoning, text, toolStart, toolEnd, finish, append([]byte("data: [DONE]"), 13, 10, 13, 10)))
	s.SetHost(h.call)

	var req ExecutorRequest
	if err := json.Unmarshal(executorRequest("client-claude"), &req); err != nil {
		t.Fatal(err)
	}
	req.Format = "claude"
	req.SourceFormat = "chat-completions"
	req.Metadata = map[string]any{"request_path": "/v1/messages"}
	req.OriginalRequest = jsonBytes(map[string]any{
		"model":  "deepseek-flash",
		"stream": true,
		"tools": []any{map[string]any{
			"name": "LookupTool",
		}},
	})

	if _, err := s.Handle("executor.execute_stream", jsonBytes(req)); err != nil {
		t.Fatalf("native Claude stream start: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("native Claude stream did not close")
	}

	h.mu.Lock()
	emitted := append([][]byte(nil), h.emitted...)
	closeError := h.clientError
	h.mu.Unlock()
	if closeError != "" {
		t.Fatalf("native Claude stream closed with error %q", closeError)
	}

	wantEvents := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(emitted) != len(wantEvents) {
		t.Fatalf("emitted %d Claude events, want %d: %#v", len(emitted), len(wantEvents), emitted)
	}
	bodies := make([]map[string]any, len(emitted))
	for i, payload := range emitted {
		event, body := decodeClaudeEventForTest(t, payload)
		if event != wantEvents[i] {
			t.Fatalf("event %d = %q, want %q", i, event, wantEvents[i])
		}
		if strings.Contains(string(payload), ""choices"") {
			t.Fatalf("event %d leaked OpenAI chunk instead of native Claude SSE: %s", i, payload)
		}
		bodies[i] = body
	}
	if object(bodies[2]["delta"])["thinking"] != "think" {
		t.Fatalf("thinking delta = %#v", bodies[2])
	}
	if object(bodies[5]["delta"])["text"] != "hello" {
		t.Fatalf("text delta = %#v", bodies[5])
	}
	tool := object(bodies[7]["content_block"])
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "LookupTool" {
		t.Fatalf("tool start = %#v", bodies[7])
	}
	if object(bodies[8]["delta"])["partial_json"] != "{"q":"x"}" {
		t.Fatalf("tool JSON delta = %#v", bodies[8])
	}
	messageDelta := bodies[10]
	if object(messageDelta["delta"])["stop_reason"] != "tool_use" {
		t.Fatalf("message stop reason = %#v", messageDelta)
	}
	usage := object(messageDelta["usage"])
	if number(usage["input_tokens"]) != 10 || number(usage["output_tokens"]) != 7 ||
		number(usage["cache_read_input_tokens"]) != 8 || number(usage["cache_creation_input_tokens"]) != 2 {
		t.Fatalf("message usage = %#v", usage)
	}
	if len(s.logs) != 1 || s.logs[0].Status != 200 || s.logs[0].OutputFormat != "claude" ||
		s.logs[0].Provider != "unknown" && s.logs[0].Provider != "deepseek" {
		t.Fatalf("native Claude stream log = %#v", s.logs)
	}
	if s.logs[0].EmitCalls != int64(len(emitted)) {
		t.Fatalf("emit calls = %d, want %d", s.logs[0].EmitCalls, len(emitted))
	}
}

func TestNativeClaudeStreamingPropagatesClientCancellation(t *testing.T) {
	s := registeredService(t, "native-fallback")
	h := newFakeHost(ssePlan(simpleSSE()))
	h.emitFailAt = 2
	s.SetHost(h.call)

	var req ExecutorRequest
	if err := json.Unmarshal(executorRequest("client-claude-cancel"), &req); err != nil {
		t.Fatal(err)
	}
	req.Format = "claude"
	req.SourceFormat = "chat-completions"
	req.Metadata = map[string]any{"request_path": "/v1/messages"}
	req.OriginalRequest = jsonBytes(map[string]any{"model": "deepseek-flash", "stream": true})

	if _, err := s.Handle("executor.execute_stream", jsonBytes(req)); err != nil {
		t.Fatalf("native Claude cancel stream start: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("native Claude canceled stream did not close")
	}
	h.mu.Lock()
	closeError := h.clientError
	emitted := append([][]byte(nil), h.emitted...)
	h.mu.Unlock()
	if len(emitted) != 1 || !strings.Contains(closeError, "client disconnected") {
		t.Fatalf("cancel result emitted=%d close=%q", len(emitted), closeError)
	}
	if len(s.logs) != 1 || s.logs[0].Status != 499 {
		t.Fatalf("cancel log = %#v", s.logs)
	}
}
