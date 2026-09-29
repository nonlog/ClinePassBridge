package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeResponsesEventForTest(t *testing.T, payload []byte) (string, map[string]any) {
	t.Helper()
	text := string(payload)
	if strings.Count(text, "event: ") != 1 {
		t.Fatalf("Responses chunk must contain exactly one SSE event, got %q", text)
	}
	var event string
	var data []byte
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	if event == "" || len(data) == 0 {
		t.Fatalf("invalid Responses SSE event: %q", text)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("invalid Responses SSE JSON for %s: %v", event, err)
	}
	return event, body
}

func TestRegistrationDeclaresNativeResponsesOutputOnly(t *testing.T) {
	reg := registration().(map[string]any)
	capabilities := reg["capabilities"].(map[string]any)
	inputs := capabilities["executor_input_formats"].([]string)
	if len(inputs) != 1 || inputs[0] != "chat-completions" {
		t.Fatalf("executor input formats = %#v, want only chat-completions for the Claude input A/B", inputs)
	}
	outputs := capabilities["executor_output_formats"].([]string)
	want := map[string]bool{"chat-completions": false, "claude": false, "openai-response": false}
	for _, format := range outputs {
		if _, ok := want[format]; ok {
			want[format] = true
		}
	}
	for format, found := range want {
		if !found {
			t.Fatalf("executor output formats = %#v, missing %s", outputs, format)
		}
	}
}

func TestNativeResponsesStreamTextReasoningAndUsage(t *testing.T) {
	request := jsonBytes(map[string]any{
		"model": "deepseek-v4.1-flash",
		"reasoning": map[string]any{"effort": "high"},
	})
	c := newResponsesStreamConverter("deepseek-v4.1-flash", request, []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	frames := []map[string]any{
		{"id": "chatcmpl-1", "created": 123, "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"reasoning_content": "think "},
		}}},
		{"id": "chatcmpl-1", "created": 123, "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "hello"},
		}}},
		{"id": "chatcmpl-1", "created": 123, "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": " world"}, "finish_reason": "stop",
		}}},
		{"id": "chatcmpl-1", "choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 100, "completion_tokens": 9, "total_tokens": 109,
			"prompt_tokens_details": map[string]any{"cached_tokens": 80},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 3},
		}},
	}
	var events [][]byte
	for _, frame := range frames {
		got, err := c.Feed(jsonBytes(frame))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, got...)
	}
	tail, err := c.Done()
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, tail...)

	var eventNames []string
	var completed map[string]any
	for _, event := range events {
		name, body := decodeResponsesEventForTest(t, event)
		eventNames = append(eventNames, name)
		if strings.Contains(string(event), `"choices"`) {
			t.Fatalf("native Responses event leaked Chat Completions choices: %s", event)
		}
		if name == "response.completed" {
			completed = body
		}
	}
	joined := strings.Join(eventNames, ",")
	for _, want := range []string{
		"response.created", "response.in_progress", "response.reasoning_summary_text.delta",
		"response.output_text.delta", "response.output_text.done", "response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events %s missing %s", joined, want)
		}
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	response := object(completed["response"])
	usage := object(response["usage"])
	if number(usage["input_tokens"]) != 100 || number(usage["output_tokens"]) != 9 {
		t.Fatalf("completed usage = %#v", usage)
	}
	if number(object(usage["input_tokens_details"])["cached_tokens"]) != 80 {
		t.Fatalf("cached usage = %#v", usage)
	}
	output := list(response["output"])
	if len(output) != 2 {
		t.Fatalf("completed output = %#v, want reasoning + message", output)
	}
}

func TestNativeResponsesStreamRestoresToolIdentity(t *testing.T) {
	original := jsonBytes(map[string]any{
		"model": "deepseek-v4.1-flash",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "mcp__server",
			"tools": []any{map[string]any{"type": "function", "name": "read_file"}},
		}},
	})
	translated := jsonBytes(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{"name": "mcp__server__read_file"},
		}},
	})
	c := newResponsesStreamConverter("deepseek-v4.1-flash", original, translated)
	frames := []map[string]any{
		{"id": "chatcmpl-tool", "created": 123, "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "function": map[string]any{
					"name": "mcp__server__read_file", "arguments": `{"pa`,
				},
			}}},
		}}},
		{"id": "chatcmpl-tool", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": `th":"a"}`},
			}}}, "finish_reason": "tool_calls",
		}}},
	}
	var events [][]byte
	for _, frame := range frames {
		got, err := c.Feed(jsonBytes(frame))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, got...)
	}
	tail, err := c.Done()
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, tail...)

	foundAdded, foundDone := false, false
	for _, event := range events {
		name, body := decodeResponsesEventForTest(t, event)
		switch name {
		case "response.output_item.added":
			item := object(body["item"])
			if item["type"] == "function_call" {
				foundAdded = true
				if item["name"] != "read_file" || item["namespace"] != "mcp__server" {
					t.Fatalf("tool identity not restored: %#v", item)
				}
			}
		case "response.function_call_arguments.done":
			foundDone = true
			if body["arguments"] != `{"path":"a"}` {
				t.Fatalf("tool arguments = %#v", body["arguments"])
			}
		}
	}
	if !foundAdded || !foundDone {
		t.Fatalf("tool events incomplete: added=%v done=%v", foundAdded, foundDone)
	}
}

func TestNativeResponsesNonstream(t *testing.T) {
	request := jsonBytes(map[string]any{"model": "deepseek-v4.1-flash"})
	body, err := openAICompletionToResponses(jsonBytes(map[string]any{
		"id": "chatcmpl-ns", "created": 321,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "done"},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
	}), "deepseek-v4.1-flash", request, []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("response = %#v", response)
	}
	output := list(response["output"])
	if len(output) != 1 || object(list(object(output[0])["content"])[0])["text"] != "done" {
		t.Fatalf("nonstream output = %#v", output)
	}
}
