package bridge

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNativeResponsesRequestBasicConversion(t *testing.T) {
	raw := jsonBytes(map[string]any{
		"model":             "client-model",
		"stream":            true,
		"instructions":      "system",
		"max_output_tokens": 2048,
		"reasoning":          map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "think"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "description": "look", "parameters": map[string]any{"type": "object"}}},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
	})
	got, err := responsesRequestToChatNative(raw, "cline-pass/deepseek-v4.1-flash", true)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "cline-pass/deepseek-v4.1-flash" || got["stream"] != true || got["max_tokens"] != float64(2048) || got["reasoning_effort"] != "high" {
		t.Fatalf("top-level conversion mismatch: %#v", got)
	}
	messages := list(got["messages"])
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	if object(messages[0])["role"] != "system" || object(messages[1])["role"] != "user" || object(messages[2])["role"] != "assistant" || object(messages[3])["role"] != "tool" {
		t.Fatalf("message roles = %#v", messages)
	}
	assistant := object(messages[2])
	if assistant["reasoning_content"] != "think" || len(list(assistant["tool_calls"])) != 1 {
		t.Fatalf("assistant reasoning/tool call = %#v", assistant)
	}
	if object(messages[3])["content"] != "result" {
		t.Fatalf("tool output = %#v", messages[3])
	}
}

func TestResponsesInputAuditMatchesEquivalentJSON(t *testing.T) {
	original := jsonBytes(map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
	})
	actual, err := responsesRequestToChatNative(original, "m", true)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(actual)
	r := ExecutorRequest{OriginalRequest: original, Payload: payload, Stream: true, Metadata: map[string]any{"request_path": "/v1/responses"}}
	_, match, diff := auditResponsesNativeInput(r)
	if !match || diff != "" {
		t.Fatalf("audit = match:%v diff:%q actual=%#v", match, diff, actual)
	}
}

func TestResponsesFirstDiffDoesNotExposeValues(t *testing.T) {
	a := map[string]any{"messages": []any{map[string]any{"content": "secret-a"}}}
	b := map[string]any{"messages": []any{map[string]any{"content": "secret-b"}}}
	diff := firstResponsesDiff(a, b, "$")
	if diff != "$.messages[0].content:value" {
		t.Fatalf("diff = %q", diff)
	}
	if reflect.DeepEqual(a, b) {
		t.Fatal("fixture unexpectedly equal")
	}
}
