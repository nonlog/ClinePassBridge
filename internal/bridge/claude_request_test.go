package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRegistrationDeclaresNativeClaudeInput(t *testing.T) {
	reg := registration().(map[string]any)
	capabilities := reg["capabilities"].(map[string]any)
	formats := capabilities["executor_input_formats"].([]string)
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
		t.Fatalf("executor input formats = %#v, want chat-completions and claude", formats)
	}
}

func TestClaudeRequestToOpenAIPreservesAgentSemantics(t *testing.T) {
	raw := jsonBytes(map[string]any{
		"model":      "deepseek-flash",
		"max_tokens": 4096,
		"system": []any{
			map[string]any{"type": "text", "text": "system rules"},
		},
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "max"},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "inspect"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "private reasoning"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"path": "a.txt"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{map[string]any{"type": "text", "text": "file body"}}},
				map[string]any{"type": "text", "text": "continue"},
			}},
		},
		"tools": []any{map[string]any{
			"name":        "Read",
			"description": "read a file",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}},
		"tool_choice":    map[string]any{"type": "auto", "disable_parallel_tool_use": true},
		"stop_sequences": []any{"STOP"},
	})

	out, err := claudeRequestToOpenAI(raw)
	if err != nil {
		t.Fatalf("convert Claude request: %v", err)
	}
	if out["reasoning_effort"] != "max" || number(out["max_tokens"]) != 4096 {
		t.Fatalf("reasoning/max_tokens not preserved: %#v", out)
	}
	if out["tool_choice"] != "auto" || out["parallel_tool_calls"] != false {
		t.Fatalf("tool choice not preserved: %#v", out)
	}
	messages := list(out["messages"])
	if len(messages) != 5 {
		t.Fatalf("messages = %#v, want system/user/assistant/tool/user", messages)
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "user"}
	for i, want := range wantRoles {
		if got := str(object(messages[i])["role"]); got != want {
			t.Fatalf("message %d role = %q, want %q", i, got, want)
		}
	}
	assistant := object(messages[2])
	if _, leaked := assistant["reasoning_content"]; leaked {
		t.Fatalf("unsigned Claude thinking leaked into OpenAI history: %#v", assistant)
	}
	calls := list(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	function := object(object(calls[0])["function"])
	if str(function["name"]) != "Read" {
		t.Fatalf("tool name = %#v", function)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(str(function["arguments"])), &args); err != nil || str(args["path"]) != "a.txt" {
		t.Fatalf("tool arguments = %q, err=%v", str(function["arguments"]), err)
	}
	toolResult := object(messages[3])
	if str(toolResult["tool_call_id"]) != "toolu_1" || str(toolResult["name"]) != "Read" || str(toolResult["content"]) != "file body" {
		t.Fatalf("tool result = %#v", toolResult)
	}
	tools := list(out["tools"])
	if len(tools) != 1 || str(object(object(tools[0])["function"])["name"]) != "Read" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestPrepareAcceptsNativeClaudeInput(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	req := ExecutorRequest{
		Model:        "deepseek-flash",
		SourceFormat: "claude",
		Format:       "claude",
		Payload: jsonBytes(map[string]any{
			"model": "deepseek-flash",
			"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}},
			}},
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": "max"},
		}),
		StorageJSON: jsonBytes(Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}),
	}
	payload, _, upstream, err := s.prepare(req)
	if err != nil {
		t.Fatalf("prepare native Claude request: %v", err)
	}
	if upstream != "cline-pass/deepseek-v4.1-flash" || str(payload["model"]) != upstream || payload["reasoning_effort"] != "max" {
		t.Fatalf("prepared request = upstream %q payload %#v", upstream, payload)
	}
	messages := list(payload["messages"])
	if len(messages) != 1 || str(object(messages[0])["role"]) != "user" {
		t.Fatalf("prepared messages = %#v", messages)
	}
}

func TestReadTimeoutMeasuresIdleGapNotWholeStream(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	s.mu.Lock()
	s.cfg.TimeoutSeconds = 1
	s.mu.Unlock()
	reads := 0
	s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.stream_read":
			reads++
			if reads <= 3 {
				time.Sleep(400 * time.Millisecond)
				*out.(*readChunk) = readChunk{Payload: []byte("x")}
				return nil
			}
			*out.(*readChunk) = readChunk{Done: true}
			return nil
		case "host.http.stream_close":
			return nil
		default:
			return nil
		}
	})
	start := time.Now()
	if err := s.read(upstreamStream{StreamID: "idle-test"}, func([]byte) error { return nil }); err != nil {
		t.Fatalf("active stream hit total-duration timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1100*time.Millisecond {
		t.Fatalf("test stream ended too quickly to cover the old absolute timeout: %v", elapsed)
	}
}

func TestTimeoutErrorsAreRequestScopedWithoutCredentialCooldown(t *testing.T) {
	rules := requestErrorRules()
	for _, status := range []int{500, 504} {
		found := false
		for _, rule := range rules {
			if rule.Status == status && rule.Action == "stop" {
				for _, match := range rule.Match {
					if match == "Cline upstream request timed out" {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing request-scoped timeout rule for status %d: %#v", status, rules)
		}
	}
}
