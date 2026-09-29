package bridge

import (
	"testing"
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

func TestClaudeInputConversionPreservesToolsResultsAndEffort(t *testing.T) {
	request := map[string]any{
		"model":      "client-model",
		"max_tokens": 2048,
		"stream":     true,
		"system":     []any{map[string]any{"type": "text", "text": "system text", "cache_control": map[string]any{"type": "ephemeral"}}},
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 12000},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "prior reasoning"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "ReadFile", "input": map[string]any{"path": "a.txt"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
				map[string]any{"type": "text", "text": "continue"},
			}},
		},
		"tools": []any{map[string]any{
			"name": "ReadFile", "description": "read",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
		"tool_choice": map[string]any{"type": "any", "disable_parallel_tool_use": true},
	}

	out, err := claudeRequestToOpenAI(jsonBytes(request), "cline-pass/deepseek-v4.1-flash", true)
	if err != nil {
		t.Fatalf("convert Claude request: %v", err)
	}
	if out["model"] != "cline-pass/deepseek-v4.1-flash" || out["stream"] != true || out["reasoning_effort"] != "high" {
		t.Fatalf("model/stream/effort mismatch: %#v", out)
	}
	messages := list(out["messages"])
	if len(messages) != 4 {
		t.Fatalf("converted message count = %d, want 4: %#v", len(messages), messages)
	}
	if object(messages[0])["role"] != "system" || object(messages[1])["role"] != "assistant" || object(messages[2])["role"] != "tool" || object(messages[3])["role"] != "user" {
		t.Fatalf("converted roles = %#v", messages)
	}
	assistant := object(messages[1])
	if assistant["reasoning_content"] != "prior reasoning" {
		t.Fatalf("reasoning content lost: %#v", assistant)
	}
	calls := list(assistant["tool_calls"])
	if len(calls) != 1 || str(object(object(calls[0])["function"])["name"]) != "ReadFile" {
		t.Fatalf("tool call lost: %#v", assistant)
	}
	toolMessage := object(messages[2])
	if toolMessage["tool_call_id"] != "toolu_1" || toolMessage["name"] != "ReadFile" || toolMessage["content"] != "ok" {
		t.Fatalf("tool result mismatch: %#v", toolMessage)
	}
	tools := list(out["tools"])
	if len(tools) != 1 || str(object(object(tools[0])["function"])["name"]) != "ReadFile" {
		t.Fatalf("tools mismatch: %#v", out["tools"])
	}
	if out["tool_choice"] != "required" || out["parallel_tool_calls"] != false {
		t.Fatalf("tool choice mismatch: %#v", out)
	}
}

func TestClaudeInputPrefersExplicitReasoningEffort(t *testing.T) {
	out, err := claudeRequestToOpenAI(jsonBytes(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"reasoning_effort": "max",
	}), "cline-pass/deepseek-v4.1-flash", true)
	if err != nil {
		t.Fatal(err)
	}
	if out["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %#v, want max", out["reasoning_effort"])
	}
}

func TestClaudeInputRequiresMessages(t *testing.T) {
	_, err := claudeRequestToOpenAI([]byte(`{"model":"x"}`), "x", true)
	if err == nil || statusOf(err) != 400 {
		t.Fatalf("missing messages error = %v status=%d, want 400", err, statusOf(err))
	}
}
