package bridge

import (
	"encoding/json"
	"strings"
)

func claudeInputRequested(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic":
		return true
	default:
		return false
	}
}

func claudeRequestToOpenAI(raw []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, fail(400, "invalid Claude request JSON")
	}

	out := map[string]any{"messages": []any{}}
	copyIfPresent := func(key string) {
		if value, ok := root[key]; ok && value != nil {
			out[key] = value
		}
	}
	for _, key := range []string{"model", "max_tokens", "temperature", "user"} {
		copyIfPresent(key)
	}
	if _, ok := out["temperature"]; !ok {
		copyIfPresent("top_p")
	}
	if stops := list(root["stop_sequences"]); len(stops) > 0 {
		out["stop"] = stops
	}
	if effort := claudeReasoningEffort(root); effort != "" {
		out["reasoning_effort"] = effort
	}

	messages := make([]any, 0, len(list(root["messages"]))+1)
	if system := claudeSystemMessage(root["system"]); system != nil {
		messages = append(messages, system)
	}
	toolNames := map[string]string{}
	for _, value := range list(root["messages"]) {
		message := object(value)
		role := strings.TrimSpace(str(message["role"]))
		if role == "" {
			continue
		}
		converted := claudeMessageToOpenAI(role, message["content"], toolNames)
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return nil, fail(400, "messages must be a nonempty array")
	}
	out["messages"] = messages

	if tools := claudeToolsToOpenAI(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, parallel, ok := claudeToolChoiceToOpenAI(root["tool_choice"]); ok {
		out["tool_choice"] = choice
		if parallel != nil {
			out["parallel_tool_calls"] = *parallel
		}
	}
	return out, nil
}

func claudeSystemMessage(value any) map[string]any {
	parts := claudeTextAndImageParts(value)
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{"role": "system", "content": parts}
}

func claudeMessageToOpenAI(role string, content any, toolNames map[string]string) []any {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"role": role, "content": text}}
	}
	parts := list(content)
	if len(parts) == 0 {
		return nil
	}

	if role == "assistant" {
		contentParts := make([]any, 0, len(parts))
		toolCalls := make([]any, 0)
		for _, rawPart := range parts {
			part := object(rawPart)
			switch str(part["type"]) {
			case "text", "image":
				if converted := claudeContentPartToOpenAI(part); converted != nil {
					contentParts = append(contentParts, converted)
				}
			case "tool_use":
				id := str(part["id"])
				name := str(part["name"])
				if id != "" && name != "" {
					toolNames[id] = name
				}
				input := part["input"]
				if input == nil {
					input = map[string]any{}
				}
				arguments := string(jsonBytes(input))
				toolCalls = append(toolCalls, map[string]any{
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				})
			}
		}
		if len(contentParts) == 0 && len(toolCalls) == 0 {
			return nil
		}
		message := map[string]any{"role": "assistant"}
		if len(contentParts) > 0 {
			message["content"] = contentParts
		} else {
			message["content"] = ""
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		return []any{message}
	}

	if role == "user" {
		out := make([]any, 0, len(parts)+1)
		contentParts := make([]any, 0, len(parts))
		var relayedImages []any
		for _, rawPart := range parts {
			part := object(rawPart)
			if str(part["type"]) != "tool_result" {
				if converted := claudeContentPartToOpenAI(part); converted != nil {
					contentParts = append(contentParts, converted)
				}
				continue
			}
			toolUseID := str(part["tool_use_id"])
			toolContent, images := claudeToolResultContent(part["content"])
			toolMessage := map[string]any{"role": "tool", "tool_call_id": toolUseID, "content": toolContent}
			if name := toolNames[toolUseID]; name != "" {
				toolMessage["name"] = name
			}
			out = append(out, toolMessage)
			relayedImages = append(relayedImages, images...)
		}
		if len(relayedImages) > 0 {
			contentParts = append(relayedImages, contentParts...)
		}
		if len(contentParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": contentParts})
		}
		return out
	}

	contentParts := claudeTextAndImageParts(parts)
	if len(contentParts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": role, "content": contentParts}}
}

func claudeTextAndImageParts(value any) []any {
	if text, ok := value.(string); ok {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": text}}
	}
	parts := list(value)
	out := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		if converted := claudeContentPartToOpenAI(object(rawPart)); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

func claudeContentPartToOpenAI(part map[string]any) any {
	switch str(part["type"]) {
	case "text":
		text := str(part["text"])
		if text == "" {
			return nil
		}
		return map[string]any{"type": "text", "text": text}
	case "image":
		source := object(part["source"])
		var imageURL string
		switch str(source["type"]) {
		case "base64":
			mediaType := str(source["media_type"])
			if mediaType == "" {
				mediaType = "application/octet-stream"
			}
			if data := str(source["data"]); data != "" {
				imageURL = "data:" + mediaType + ";base64," + data
			}
		case "url":
			imageURL = str(source["url"])
		}
		if imageURL == "" {
			imageURL = str(part["url"])
		}
		if imageURL == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}
	default:
		return nil
	}
}

func claudeToolResultContent(value any) (string, []any) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	var texts []string
	var images []any
	for _, rawPart := range list(value) {
		part := object(rawPart)
		switch str(part["type"]) {
		case "text":
			if text := str(part["text"]); text != "" {
				texts = append(texts, text)
			}
		case "image":
			if image := claudeContentPartToOpenAI(part); image != nil {
				images = append(images, image)
			}
		}
	}
	content := strings.Join(texts, "
")
	if content == "" && len(images) > 0 {
		content = "[Tool returned image content; the images follow in the next user message.]"
	}
	return content, images
}

func claudeToolsToOpenAI(value any) []any {
	tools := list(value)
	out := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool := object(rawTool)
		name := strings.TrimSpace(str(tool["name"]))
		if name == "" {
			continue
		}
		parameters := normalizeClaudeObjectSchema(tool["input_schema"])
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		function := map[string]any{"name": name, "parameters": parameters}
		if description := str(tool["description"]); description != "" {
			function["description"] = description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

func normalizeClaudeObjectSchema(value any) any {
	if value == nil {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	copy := make(map[string]any, len(schema)+1)
	for key, child := range schema {
		copy[key] = child
	}
	if str(copy["type"]) == "object" {
		if _, ok := copy["properties"]; !ok {
			copy["properties"] = map[string]any{}
		}
	}
	return copy
}

func claudeToolChoiceToOpenAI(value any) (any, *bool, bool) {
	choice := object(value)
	if choice == nil {
		if text, ok := value.(string); ok && text != "" {
			choice = map[string]any{"type": text}
		} else {
			return nil, nil, false
		}
	}
	var out any
	switch str(choice["type"]) {
	case "auto":
		out = "auto"
	case "any":
		out = "required"
	case "none":
		out = "none"
	case "tool":
		name := str(choice["name"])
		if name == "" {
			out = "none"
		} else {
			out = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	default:
		out = "none"
	}
	var parallel *bool
	if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok && disabled {
		value := false
		parallel = &value
	}
	return out, parallel, true
}

func claudeReasoningEffort(root map[string]any) string {
	thinking := object(root["thinking"])
	if thinking == nil {
		return ""
	}
	switch strings.ToLower(str(thinking["type"])) {
	case "disabled":
		return "none"
	case "adaptive", "auto":
		if effort := strings.ToLower(strings.TrimSpace(str(object(root["output_config"])["effort"]))); effort != "" {
			return effort
		}
		return "xhigh"
	case "enabled":
		if _, ok := thinking["budget_tokens"]; !ok {
			return "auto"
		}
		budget := number(thinking["budget_tokens"])
		return reasoningEffortFromBudget(budget)
	default:
		return ""
	}
}

func reasoningEffortFromBudget(budget int64) string {
	switch {
	case budget < 0:
		return "auto"
	case budget == 0:
		return "none"
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
}
