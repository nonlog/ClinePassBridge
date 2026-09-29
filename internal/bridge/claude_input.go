package bridge

import (
	"encoding/json"
	"strings"
)

func claudeInputRequested(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic", "messages":
		return true
	default:
		return false
	}
}

func claudeRequestToOpenAI(raw []byte, model string, stream bool) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, fail(400, "invalid Claude request JSON")
	}
	if len(list(root["messages"])) == 0 {
		return nil, fail(400, "messages must be a nonempty array")
	}

	out := map[string]any{
		"model":    model,
		"messages": []any{},
		"stream":   stream,
	}
	for _, key := range []string{"max_tokens", "user"} {
		if value, ok := root[key]; ok {
			out[key] = value
		}
	}
	if value, ok := root["temperature"]; ok {
		out["temperature"] = value
	} else if value, ok := root["top_p"]; ok {
		out["top_p"] = value
	}
	if stops := list(root["stop_sequences"]); len(stops) > 0 {
		out["stop"] = stops
	}
	if value, ok := root["provider"]; ok {
		out["provider"] = value
	}
	if value, ok := root["providerOptions"]; ok {
		out["providerOptions"] = value
	}

	if effort := strings.TrimSpace(str(root["reasoning_effort"])); effort != "" {
		out["reasoning_effort"] = effort
	} else if effort := claudeThinkingEffort(root); effort != "" {
		out["reasoning_effort"] = effort
	}

	messages := make([]any, 0, len(list(root["messages"]))+1)
	if systemParts := claudeSystemParts(root["system"]); len(systemParts) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": systemParts})
	}

	toolNameByID := map[string]string{}
	for _, rawMessage := range list(root["messages"]) {
		message := object(rawMessage)
		role := str(message["role"])
		if role == "" {
			continue
		}
		content := message["content"]
		if text, ok := content.(string); ok {
			if text != "" {
				messages = append(messages, map[string]any{"role": role, "content": text})
			}
			continue
		}

		parts := list(content)
		switch role {
		case "assistant":
			openAIParts := make([]any, 0, len(parts))
			toolCalls := make([]any, 0)
			for _, rawPart := range parts {
				part := object(rawPart)
				switch str(part["type"]) {
				case "text", "image":
					if converted, ok := claudeContentPart(part); ok {
						openAIParts = append(openAIParts, converted)
					}
				case "tool_use":
					id := str(part["id"])
					name := str(part["name"])
					if id != "" && name != "" {
						toolNameByID[id] = name
					}
					args := "{}"
					if input, ok := part["input"]; ok && input != nil {
						if encoded, err := json.Marshal(input); err == nil {
							args = string(encoded)
						}
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": args,
						},
					})
				}
			}
			if len(openAIParts) == 0 && len(toolCalls) == 0 {
				continue
			}
			converted := map[string]any{"role": "assistant"}
			if len(openAIParts) > 0 {
				converted["content"] = openAIParts
			} else {
				converted["content"] = ""
			}
			if len(toolCalls) > 0 {
				converted["tool_calls"] = toolCalls
			}
			messages = append(messages, converted)

		case "user":
			contentParts := make([]any, 0, len(parts))
			toolMessages := make([]any, 0)
			relayedImages := make([]any, 0)
			for _, rawPart := range parts {
				part := object(rawPart)
				switch str(part["type"]) {
				case "tool_result":
					toolUseID := str(part["tool_use_id"])
					text, images := claudeToolResultContent(part["content"])
					toolMessage := map[string]any{
						"role":         "tool",
						"tool_call_id": toolUseID,
						"content":      text,
					}
					if name := toolNameByID[toolUseID]; name != "" {
						toolMessage["name"] = name
					}
					toolMessages = append(toolMessages, toolMessage)
					relayedImages = append(relayedImages, images...)
				case "text", "image":
					if converted, ok := claudeContentPart(part); ok {
						contentParts = append(contentParts, converted)
					}
				}
			}
			messages = append(messages, toolMessages...)
			if len(relayedImages) > 0 {
				prefix := []any{map[string]any{"type": "text", "text": "Images returned by the preceding tool call(s):"}}
				prefix = append(prefix, relayedImages...)
				contentParts = append(prefix, contentParts...)
			}
			if len(contentParts) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": contentParts})
			}

		default:
			contentParts := make([]any, 0, len(parts))
			for _, rawPart := range parts {
				if converted, ok := claudeContentPart(object(rawPart)); ok {
					contentParts = append(contentParts, converted)
				}
			}
			if len(contentParts) > 0 {
				messages = append(messages, map[string]any{"role": role, "content": contentParts})
			}
		}
	}
	if len(messages) == 0 {
		return nil, fail(400, "Claude request produced no upstream messages")
	}
	out["messages"] = messages

	if tools := list(root["tools"]); len(tools) > 0 {
		convertedTools := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			tool := object(rawTool)
			name := str(tool["name"])
			if name == "" {
				continue
			}
			parameters := tool["input_schema"]
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			convertedTools = append(convertedTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": str(tool["description"]),
					"parameters":  parameters,
				},
			})
		}
		if len(convertedTools) > 0 {
			out["tools"] = convertedTools
		}
	}

	if choice := object(root["tool_choice"]); choice != nil {
		switch str(choice["type"]) {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if name := str(choice["name"]); name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok && disabled {
			out["parallel_tool_calls"] = false
		}
	}
	return out, nil
}

func claudeSystemParts(value any) []any {
	parts := make([]any, 0)
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			parts = append(parts, map[string]any{"type": "text", "text": typed})
		}
	case []any:
		for _, rawPart := range typed {
			if converted, ok := claudeContentPart(object(rawPart)); ok {
				if object(converted)["type"] == "text" {
					parts = append(parts, converted)
				}
			}
		}
	}
	return parts
}

func claudeContentPart(part map[string]any) (any, bool) {
	switch str(part["type"]) {
	case "text":
		text := str(part["text"])
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		return map[string]any{"type": "text", "text": text}, true
	case "image":
		source := object(part["source"])
		var imageURL string
		switch str(source["type"]) {
		case "base64":
			data := str(source["data"])
			if data != "" {
				mediaType := str(source["media_type"])
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				imageURL = "data:" + mediaType + ";base64," + data
			}
		case "url":
			imageURL = str(source["url"])
		}
		if imageURL == "" {
			imageURL = str(part["url"])
		}
		if imageURL == "" {
			return nil, false
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}, true
	default:
		return nil, false
	}
}

func claudeToolResultContent(value any) (string, []any) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []any:
		texts := make([]string, 0, len(typed))
		images := make([]any, 0)
		for _, rawPart := range typed {
			if text, ok := rawPart.(string); ok {
				texts = append(texts, text)
				continue
			}
			part := object(rawPart)
			switch str(part["type"]) {
			case "text":
				texts = append(texts, str(part["text"]))
			case "image":
				if converted, ok := claudeContentPart(part); ok {
					images = append(images, converted)
				}
			default:
				if encoded, err := json.Marshal(rawPart); err == nil {
					texts = append(texts, string(encoded))
				}
			}
		}
		text := strings.Join(texts, "\n\n")
		if strings.TrimSpace(text) == "" && len(images) > 0 {
			text = "[Tool returned image content; the images follow in the next user message.]"
		}
		return text, images
	default:
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded), nil
		}
		return "", nil
	}
}

func claudeThinkingEffort(root map[string]any) string {
	thinking := object(root["thinking"])
	switch str(thinking["type"]) {
	case "disabled":
		return "none"
	case "adaptive", "auto":
		if effort := strings.TrimSpace(str(object(root["output_config"])["effort"])); effort != "" {
			return effort
		}
		return "xhigh"
	case "enabled":
		budget := int(number(thinking["budget_tokens"]))
		switch {
		case budget == 0:
			return "auto"
		case budget <= 512:
			return "minimal"
		case budget <= 1024:
			return "low"
		case budget <= 8192:
			return "medium"
		case budget <= 24576:
			return "high"
		default:
			return "xhigh"
		}
	default:
		return ""
	}
}
