package translator

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeToChatCompletions converts Claude Messages format to Chat Completions format.
// This is a high-performance, cache-preserving port of CPA's translator logic.
//
// Critical: Must maintain exact semantic parity with CPA's translator to preserve prompt cache.
// Any deviation in system/message structure, thinking handling, or tool conversion will break cache.
//
// Performance target: <100ms for 350k token requests (vs 3-5s in CPA's implementation).
func ConvertClaudeToChatCompletions(modelName string, rawJSON []byte, stream bool) ([]byte, error) {
	// Base template
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)

	root := gjson.ParseBytes(rawJSON)

	// Derive user_id for metadata
	userID := deriveUserID(root)
	out, _ = sjson.SetBytes(out, "metadata.user_id", userID)

	// Set model
	out, _ = sjson.SetBytes(out, "model", modelName)
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Map max_tokens
	if mt := root.Get("max_tokens"); mt.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", mt.Int())
	}

	// Map temperature
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	}

	// Map top_p
	if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	// Map top_k
	if topK := root.Get("top_k"); topK.Exists() {
		out, _ = sjson.SetBytes(out, "top_k", topK.Int())
	}

	// Map stop sequences
	if stop := root.Get("stop_sequences"); stop.Exists() && stop.IsArray() {
		out, _ = sjson.SetRawBytes(out, "stop", []byte(stop.Raw))
	}

	// Convert system to system message
	var messages [][]byte
	if system := root.Get("system"); system.Exists() {
		if system.IsArray() {
			// System is array of content blocks
			var systemText strings.Builder
			system.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					if systemText.Len() > 0 {
						systemText.WriteString("\n\n")
					}
					systemText.WriteString(block.Get("text").String())
				}
				return true
			})
			if systemText.Len() > 0 {
				sysMsg := []byte(`{"role":"system","content":""}`)
				sysMsg, _ = sjson.SetBytes(sysMsg, "content", systemText.String())
				messages = append(messages, sysMsg)
			}
		} else if system.Type == gjson.String {
			sysMsg := []byte(`{"role":"system","content":""}`)
			sysMsg, _ = sjson.SetBytes(sysMsg, "content", system.String())
			messages = append(messages, sysMsg)
		}
	}

	// Convert messages array
	if msgs := root.Get("messages"); msgs.Exists() && msgs.IsArray() {
		messages = convertClaudeMessages(msgs, messages)
	}

	// Set messages
	if len(messages) > 0 {
		out, _ = sjson.SetRawBytes(out, "messages", joinRawArray(messages))
	}

	// Convert tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		chatTools := convertClaudeTools(tools)
		if len(chatTools) > 0 {
			out, _ = sjson.SetBytes(out, "tools", chatTools)

			// Convert tool_choice
			if tc := root.Get("tool_choice"); tc.Exists() {
				out, _ = sjson.SetRawBytes(out, "tool_choice", convertClaudeToolChoice(tc))
			}
		}
	}

	// Map thinking/reasoning_effort
	if thinking := root.Get("thinking"); thinking.Exists() {
		if thinkingType := thinking.Get("type"); thinkingType.Exists() {
			switch thinkingType.String() {
			case "disabled":
				out, _ = sjson.SetBytes(out, "reasoning_effort", "none")
			case "adaptive":
				out, _ = sjson.SetBytes(out, "reasoning_effort", "auto")
			case "enabled":
				// Budget-based thinking - map to medium effort
				out, _ = sjson.SetBytes(out, "reasoning_effort", "medium")
			}
		}
	}

	return out, nil
}

func deriveUserID(root gjson.Result) string {
	// Try metadata.user_id first
	if userID := root.Get("metadata.user_id"); userID.Exists() {
		return userID.String()
	}
	// Try anthropic-user-id header equivalent
	if userID := root.Get("user_id"); userID.Exists() {
		return userID.String()
	}
	return "default-user"
}

func convertClaudeMessages(msgs gjson.Result, existing [][]byte) [][]byte {
	messages := existing

	// Track pending tool calls to batch into assistant messages
	var pendingToolCalls []interface{}
	var pendingThinking string

	flushToolCalls := func() {
		if len(pendingToolCalls) == 0 {
			return
		}

		assistantMsg := []byte(`{"role":"assistant","tool_calls":[]}`)
		assistantMsg, _ = sjson.SetBytes(assistantMsg, "tool_calls", pendingToolCalls)

		if pendingThinking != "" {
			assistantMsg, _ = sjson.SetBytes(assistantMsg, "reasoning_content", pendingThinking)
			pendingThinking = ""
		}

		messages = append(messages, assistantMsg)
		pendingToolCalls = nil
	}

	msgs.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		switch role {
		case "user":
			flushToolCalls()

			userMsg := []byte(`{"role":"user","content":[]}`)
			var contentItems [][]byte

			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					blockType := block.Get("type").String()
					switch blockType {
					case "text":
						part := []byte(`{"type":"text","text":""}`)
						part, _ = sjson.SetBytes(part, "text", block.Get("text").String())
						contentItems = append(contentItems, part)
					case "image":
						part := []byte(`{"type":"image_url","image_url":{"url":""}}`)
						source := block.Get("source")
						if source.Get("type").String() == "base64" {
							mediaType := source.Get("media_type").String()
							data := source.Get("data").String()
							url := "data:" + mediaType + ";base64," + data
							part, _ = sjson.SetBytes(part, "image_url.url", url)
						}
						contentItems = append(contentItems, part)
					case "tool_use":
						// Shouldn't appear in user messages, but handle gracefully
						// Convert to text representation
						part := []byte(`{"type":"text","text":""}`)
						text := "[tool_use: " + block.Get("name").String() + "]"
						part, _ = sjson.SetBytes(part, "text", text)
						contentItems = append(contentItems, part)
					case "tool_result":
						// This is actually a tool message in Chat Completions
						flushToolCalls()

						toolMsg := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
						toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", block.Get("tool_use_id").String())

						if toolContent := block.Get("content"); toolContent.Exists() {
							if toolContent.IsArray() {
								var textParts []string
								toolContent.ForEach(func(_, tcBlock gjson.Result) bool {
									if tcBlock.Get("type").String() == "text" {
										textParts = append(textParts, tcBlock.Get("text").String())
									}
									return true
								})
								toolMsg, _ = sjson.SetBytes(toolMsg, "content", strings.Join(textParts, "\n"))
							} else {
								toolMsg, _ = sjson.SetBytes(toolMsg, "content", toolContent.String())
							}
						}

						messages = append(messages, toolMsg)
						return true
					}
					return true
				})
			} else if content.Type == gjson.String {
				part := []byte(`{"type":"text","text":""}`)
				part, _ = sjson.SetBytes(part, "text", content.String())
				contentItems = append(contentItems, part)
			}

			userMsg = setRawArrayItems(userMsg, "content", contentItems)
			messages = append(messages, userMsg)

		case "assistant":
			var textContent strings.Builder
			var thinkingContent string

			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					blockType := block.Get("type").String()
					switch blockType {
					case "text":
						if textContent.Len() > 0 {
							textContent.WriteString("\n\n")
						}
						textContent.WriteString(block.Get("text").String())
					case "thinking":
						// Unsigned thinking from prior turn
						thinkingContent = block.Get("thinking").String()
					case "tool_use":
						// Accumulate tool calls
						toolCall := map[string]interface{}{
							"id":   block.Get("id").String(),
							"type": "function",
							"function": map[string]interface{}{
								"name":      block.Get("name").String(),
								"arguments": block.Get("input").Raw,
							},
						}
						pendingToolCalls = append(pendingToolCalls, toolCall)
					}
					return true
				})
			} else if content.Type == gjson.String {
				textContent.WriteString(content.String())
			}

			// If we have tool calls but no text, just accumulate
			if len(pendingToolCalls) > 0 {
				if thinkingContent != "" {
					pendingThinking = thinkingContent
				}
				if textContent.Len() == 0 {
					return true
				}
			}

			// Flush any pending tool calls first
			flushToolCalls()

			// Then add text message
			if textContent.Len() > 0 {
				assistantMsg := []byte(`{"role":"assistant","content":""}`)
				assistantMsg, _ = sjson.SetBytes(assistantMsg, "content", textContent.String())
				if thinkingContent != "" {
					assistantMsg, _ = sjson.SetBytes(assistantMsg, "reasoning_content", thinkingContent)
				}
				messages = append(messages, assistantMsg)
			}
		}

		return true
	})

	flushToolCalls()
	return messages
}

func convertClaudeTools(tools gjson.Result) []interface{} {
	var chatTools []interface{}

	tools.ForEach(func(_, tool gjson.Result) bool {
		chatTool := map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": tool.Get("name").String(),
			},
		}

		if desc := tool.Get("description"); desc.Exists() {
			chatTool["function"].(map[string]interface{})["description"] = desc.String()
		}

		if inputSchema := tool.Get("input_schema"); inputSchema.Exists() {
			chatTool["function"].(map[string]interface{})["parameters"] = gjson.Parse(inputSchema.Raw).Value()
		}

		chatTools = append(chatTools, chatTool)
		return true
	})

	return chatTools
}

func convertClaudeToolChoice(toolChoice gjson.Result) []byte {
	if !toolChoice.IsObject() {
		return []byte(toolChoice.Raw)
	}

	tcType := toolChoice.Get("type").String()
	switch tcType {
	case "auto":
		return []byte(`"auto"`)
	case "any":
		return []byte(`"required"`)
	case "tool":
		name := toolChoice.Get("name").String()
		if name == "" {
			return []byte(`"auto"`)
		}
		converted := []byte(`{"type":"function","function":{"name":""}}`)
		converted, _ = sjson.SetBytes(converted, "function.name", name)
		return converted
	default:
		return []byte(`"auto"`)
	}
}
