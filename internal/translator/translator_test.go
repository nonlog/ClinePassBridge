package translator

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeToChatCompletions(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantModel  string
		wantStream bool
		wantErr    bool
	}{
		{
			name: "basic text message",
			input: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 1024,
				"messages": [
					{"role": "user", "content": "Hello"}
				]
			}`,
			wantModel:  "claude-3-5-sonnet-20241022",
			wantStream: false,
		},
		{
			name: "with system message",
			input: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 1024,
				"system": "You are a helpful assistant.",
				"messages": [
					{"role": "user", "content": "Hello"}
				]
			}`,
			wantModel:  "claude-3-5-sonnet-20241022",
			wantStream: false,
		},
		{
			name: "with tool use",
			input: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 1024,
				"tools": [
					{
						"name": "get_weather",
						"description": "Get weather",
						"input_schema": {
							"type": "object",
							"properties": {
								"location": {"type": "string"}
							}
						}
					}
				],
				"messages": [
					{"role": "user", "content": "What's the weather?"}
				]
			}`,
			wantModel:  "claude-3-5-sonnet-20241022",
			wantStream: false,
		},
		{
			name: "with tool result",
			input: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 1024,
				"messages": [
					{"role": "user", "content": "What's the weather?"},
					{
						"role": "assistant",
						"content": [
							{"type": "text", "text": "Let me check"},
							{
								"type": "tool_use",
								"id": "toolu_123",
								"name": "get_weather",
								"input": {"location": "SF"}
							}
						]
					},
					{
						"role": "user",
						"content": [
							{
								"type": "tool_result",
								"tool_use_id": "toolu_123",
								"content": "Sunny, 72F"
							}
						]
					}
				]
			}`,
			wantModel:  "claude-3-5-sonnet-20241022",
			wantStream: false,
		},
		{
			name: "with thinking",
			input: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 1024,
				"thinking": {"type": "adaptive"},
				"messages": [
					{"role": "user", "content": "Solve this"}
				]
			}`,
			wantModel:  "claude-3-5-sonnet-20241022",
			wantStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertClaudeToChatCompletions(tt.wantModel, []byte(tt.input), tt.wantStream)
			if (err != nil) != tt.wantErr {
				t.Errorf("ConvertClaudeToChatCompletions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err == nil {
				result := gjson.ParseBytes(got)
				if model := result.Get("model").String(); model != tt.wantModel {
					t.Errorf("model = %v, want %v", model, tt.wantModel)
				}
				if stream := result.Get("stream").Bool(); stream != tt.wantStream {
					t.Errorf("stream = %v, want %v", stream, tt.wantStream)
				}
				if !result.Get("messages").Exists() {
					t.Error("messages field missing")
				}
			}
		})
	}
}

func TestConvertResponsesToChatCompletions(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantModel  string
		wantStream bool
		wantErr    bool
	}{
		{
			name: "basic text input",
			input: `{
				"model": "deepseek-v4.1-flash",
				"max_output_tokens": 1024,
				"input": "Hello"
			}`,
			wantModel:  "deepseek-v4.1-flash",
			wantStream: false,
		},
		{
			name: "with instructions",
			input: `{
				"model": "deepseek-v4.1-flash",
				"max_output_tokens": 1024,
				"instructions": "You are a helpful assistant.",
				"input": "Hello"
			}`,
			wantModel:  "deepseek-v4.1-flash",
			wantStream: false,
		},
		{
			name: "message array input",
			input: `{
				"model": "deepseek-v4.1-flash",
				"max_output_tokens": 1024,
				"input": [
					{
						"type": "message",
						"role": "user",
						"content": [
							{"type": "input_text", "text": "Hello"}
						]
					}
				]
			}`,
			wantModel:  "deepseek-v4.1-flash",
			wantStream: false,
		},
		{
			name: "with reasoning effort",
			input: `{
				"model": "deepseek-v4.1-flash",
				"max_output_tokens": 1024,
				"reasoning": {"effort": "medium"},
				"input": "Solve this"
			}`,
			wantModel:  "deepseek-v4.1-flash",
			wantStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertResponsesToChatCompletions(tt.wantModel, []byte(tt.input), tt.wantStream)
			if (err != nil) != tt.wantErr {
				t.Errorf("ConvertResponsesToChatCompletions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err == nil {
				result := gjson.ParseBytes(got)
				if model := result.Get("model").String(); model != tt.wantModel {
					t.Errorf("model = %v, want %v", model, tt.wantModel)
				}
				if stream := result.Get("stream").Bool(); stream != tt.wantStream {
					t.Errorf("stream = %v, want %v", stream, tt.wantStream)
				}
				if !result.Get("messages").Exists() {
					t.Error("messages field missing")
				}
			}
		})
	}
}

func BenchmarkConvertClaudeToChatCompletions(b *testing.B) {
	// Simulate a large 300k token request
	largeInput := `{
		"model": "claude-3-5-sonnet-20241022",
		"max_tokens": 4096,
		"system": "You are a helpful assistant.",
		"messages": [`

	// Add ~100 message exchanges to simulate large context
	for i := 0; i < 100; i++ {
		if i > 0 {
			largeInput += ","
		}
		largeInput += `
			{"role": "user", "content": "` + strings.Repeat("test ", 300) + `"},
			{"role": "assistant", "content": "` + strings.Repeat("response ", 300) + `"}`
	}

	largeInput += `]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ConvertClaudeToChatCompletions("claude-3-5-sonnet-20241022", []byte(largeInput), true)
	}
}

func BenchmarkConvertResponsesToChatCompletions(b *testing.B) {
	// Simulate a large 300k token request
	largeInput := `{
		"model": "deepseek-v4.1-flash",
		"max_output_tokens": 4096,
		"instructions": "You are a helpful assistant.",
		"input": [`

	// Add ~100 message exchanges
	for i := 0; i < 100; i++ {
		if i > 0 {
			largeInput += ","
		}
		largeInput += `
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "` + strings.Repeat("test ", 300) + `"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "` + strings.Repeat("response ", 300) + `"}]
			}`
	}

	largeInput += `]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ConvertResponsesToChatCompletions("deepseek-v4.1-flash", []byte(largeInput), true)
	}
}
