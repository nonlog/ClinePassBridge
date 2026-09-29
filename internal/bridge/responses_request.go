package bridge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type responsesToolSpec struct {
	Identity responsesToolIdentity
	Tool     map[string]any
	ChatName string
}

func responsesRequestToChatNative(raw []byte, model string, stream bool) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, fail(400, "invalid Responses request JSON")
	}
	out := map[string]any{"model": model, "messages": []any{}, "stream": stream}
	if value, ok := root["max_output_tokens"]; ok {
		out["max_tokens"] = value
	}
	if text := object(root["text"]); text != nil {
		if format := object(text["format"]); format != nil {
			if converted := responsesTextFormatToChat(format); converted != nil {
				out["response_format"] = converted
			}
		}
	}

	messages := make([]any, 0)
	if instructions, ok := root["instructions"].(string); ok {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	input, hasInput := root["input"]
	if inputText, ok := input.(string); ok {
		messages = append(messages, map[string]any{"role": "user", "content": inputText})
	} else if hasInput {
		items := list(input)
		hasReasoning := responsesHasReasoning(root, items)
		pendingToolCalls := make([]any, 0)
		pendingToolCallIDs := make([]string, 0)
		pendingReasoning := ""
		latestReasoning := ""
		awaitingToolOutputs := map[string]bool{}
		mergeableAssistant := -1

		fallbackToolReasoning := func() string {
			if latestReasoning != "" {
				return latestReasoning
			}
			if hasReasoning {
				return "[reasoning unavailable]"
			}
			return ""
		}
		takePendingReasoning := func() string {
			value := pendingReasoning
			pendingReasoning = ""
			return value
		}
		flushPendingTools := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			reasoning := takePendingReasoning()
			merged := false
			if mergeableAssistant >= 0 && mergeableAssistant == len(messages)-1 {
				msg := object(messages[mergeableAssistant])
				if msg != nil && str(msg["role"]) == "assistant" && msg["tool_calls"] == nil {
					msg["tool_calls"] = append([]any(nil), pendingToolCalls...)
					combined := combineResponsesReasoning(str(msg["reasoning_content"]), reasoning)
					if combined != "" {
						msg["reasoning_content"] = combined
						if usableResponsesReasoning(combined) {
							latestReasoning = combined
						}
					} else if fallback := fallbackToolReasoning(); fallback != "" {
						msg["reasoning_content"] = fallback
					}
					merged = true
				}
			}
			if !merged {
				msg := map[string]any{"role": "assistant", "tool_calls": append([]any(nil), pendingToolCalls...)}
				if reasoning != "" {
					msg["reasoning_content"] = reasoning
					if usableResponsesReasoning(reasoning) {
						latestReasoning = reasoning
					}
				} else if fallback := fallbackToolReasoning(); fallback != "" {
					msg["reasoning_content"] = fallback
				}
				messages = append(messages, msg)
			}
			for _, callID := range pendingToolCallIDs {
				if strings.TrimSpace(callID) != "" {
					awaitingToolOutputs[callID] = true
				}
			}
			pendingToolCalls = pendingToolCalls[:0]
			pendingToolCallIDs = pendingToolCallIDs[:0]
			mergeableAssistant = -1
		}
		appendPendingReasoning := func() {
			reasoning := takePendingReasoning()
			if reasoning == "" {
				return
			}
			if usableResponsesReasoning(reasoning) {
				latestReasoning = reasoning
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": "", "reasoning_content": reasoning})
		}

		for _, rawItem := range items {
			item := object(rawItem)
			itemType := str(item["type"])
			if itemType == "" && str(item["role"]) != "" {
				itemType = "message"
			}
			if itemType != "function_call" && itemType != "custom_tool_call" {
				flushPendingTools()
			}
			switch itemType {
			case "message", "":
				role := str(item["role"])
				if role == "developer" {
					role = "user"
				}
				mergeableAssistant = -1
				if role != "assistant" {
					appendPendingReasoning()
					latestReasoning = ""
				}
				msg := map[string]any{"role": role, "content": []any{}}
				if content, ok := item["content"].(string); ok {
					msg["content"] = content
				} else {
					msg["content"] = responsesContentToChat(list(item["content"]))
				}
				if role == "assistant" {
					reasoning := combineResponsesReasoning(takePendingReasoning(), str(item["reasoning_content"]))
					if reasoning != "" {
						msg["reasoning_content"] = reasoning
						if usableResponsesReasoning(reasoning) {
							latestReasoning = reasoning
						}
					}
				}
				messages = append(messages, msg)
				if role == "assistant" {
					mergeableAssistant = len(messages) - 1
				}
			case "reasoning":
				reasoning := responsesReasoningItemText(item)
				pendingReasoning = combineResponsesReasoning(pendingReasoning, reasoning)
				if usableResponsesReasoning(reasoning) {
					latestReasoning = reasoning
				}
			case "function_call":
				reasoning := str(item["reasoning_content"])
				pendingReasoning = combineResponsesReasoning(pendingReasoning, reasoning)
				if usableResponsesReasoning(reasoning) {
					latestReasoning = reasoning
				}
				callID := responsesCallID(item)
				name := responsesRequestToolChatName(root, str(item["namespace"]), str(item["name"]))
				toolCall := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": str(item["arguments"])}}
				pendingToolCalls = append(pendingToolCalls, toolCall)
				if callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}
			case "function_call_output":
				mergeableAssistant = -1
				callID := responsesCallID(item)
				content := responsesToolOutputTextNative(item["output"])
				if callID != "" && awaitingToolOutputs[callID] {
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
					delete(awaitingToolOutputs, callID)
				} else if strings.TrimSpace(content) != "" {
					messages = append(messages, map[string]any{"role": "user", "content": content})
				}
			case "custom_tool_call":
				reasoning := str(item["reasoning_content"])
				pendingReasoning = combineResponsesReasoning(pendingReasoning, reasoning)
				if usableResponsesReasoning(reasoning) {
					latestReasoning = reasoning
				}
				callID := responsesCallID(item)
				name := responsesRequestToolChatName(root, str(item["namespace"]), str(item["name"]))
				args, _ := json.Marshal(map[string]any{"input": str(item["input"])})
				pendingToolCalls = append(pendingToolCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}})
				if callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}
			case "custom_tool_call_output":
				mergeableAssistant = -1
				callID := responsesCallID(item)
				content := responsesToolOutputTextNative(item["output"])
				if callID != "" && awaitingToolOutputs[callID] {
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
					delete(awaitingToolOutputs, callID)
				} else if strings.TrimSpace(content) != "" {
					messages = append(messages, map[string]any{"role": "user", "content": content})
				}
			}
		}
		flushPendingTools()
		appendPendingReasoning()
	}
	out["messages"] = messages

	toolSpecs := collectResponsesToolSpecs(root)
	if len(toolSpecs) > 0 {
		tools := make([]any, 0, len(toolSpecs))
		seen := map[string]bool{}
		for _, spec := range toolSpecs {
			if seen[spec.ChatName] {
				continue
			}
			seen[spec.ChatName] = true
			tools = append(tools, responsesToolSpecToChat(spec))
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if value, ok := root["parallel_tool_calls"]; ok {
				out["parallel_tool_calls"] = value
			}
			if choice, ok := root["tool_choice"]; ok {
				out["tool_choice"] = responsesToolChoiceToChat(root, choice)
			}
		}
	}
	if reasoning := object(root["reasoning"]); reasoning != nil {
		if effort := strings.ToLower(strings.TrimSpace(str(reasoning["effort"]))); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	return out, nil
}

func responsesContentToChat(parts []any) []any {
	out := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part := object(rawPart)
		typeName := str(part["type"])
		if typeName == "" {
			typeName = "input_text"
		}
		switch typeName {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": str(part["text"])})
		case "input_image":
			image := map[string]any{"url": str(part["image_url"])}
			if detail := normalizeResponsesImageDetail(str(part["detail"])); detail != "" {
				image["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		case "input_video", "video_url":
			video := object(part["video_url"])
			if video == nil {
				video = map[string]any{"url": part["video_url"]}
			}
			if processing, ok := part["processing"]; ok {
				video["processing"] = processing
			}
			out = append(out, map[string]any{"type": "video_url", "video_url": video})
		}
	}
	return out
}

func normalizeResponsesImageDetail(detail string) string {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "auto", "low", "high":
		return strings.ToLower(strings.TrimSpace(detail))
	case "original":
		return "high"
	default:
		return ""
	}
}

func responsesHasReasoning(root map[string]any, items []any) bool {
	if reasoning := object(root["reasoning"]); reasoning != nil {
		effort := strings.ToLower(strings.TrimSpace(str(reasoning["effort"])))
		if effort != "" && effort != "none" && effort != "0" && effort != "false" {
			return true
		}
	}
	if effort := strings.ToLower(strings.TrimSpace(str(root["reasoning_effort"]))); effort != "" && effort != "none" && effort != "0" && effort != "false" {
		return true
	}
	for _, raw := range items {
		item := object(raw)
		if str(item["type"]) == "reasoning" || item["reasoning_content"] != nil {
			return true
		}
	}
	return false
}

func responsesReasoningItemText(item map[string]any) string {
	var b strings.Builder
	for _, raw := range list(item["summary"]) {
		part := object(raw)
		if str(part["type"]) == "summary_text" {
			b.WriteString(str(part["text"]))
		}
	}
	if b.Len() == 0 {
		return "[reasoning unavailable]"
	}
	return b.String()
}

func combineResponsesReasoning(existing, incoming string) string {
	a, b := strings.TrimSpace(existing), strings.TrimSpace(incoming)
	switch {
	case a == "":
		return incoming
	case b == "":
		return existing
	case a == "[reasoning unavailable]":
		return incoming
	case b == "[reasoning unavailable]" || a == b:
		return existing
	default:
		return existing + "\n\n" + incoming
	}
}

func usableResponsesReasoning(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && trimmed != "[reasoning unavailable]"
}

func responsesCallID(item map[string]any) string {
	for _, key := range []string{"call_id", "tool_call_id", "callId", "id"} {
		if value := strings.TrimSpace(str(item[key])); value != "" {
			if key == "id" && strings.HasPrefix(value, "fco_") {
				continue
			}
			return value
		}
	}
	return ""
}

func responsesToolOutputTextNative(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case []any:
		var b strings.Builder
		for _, raw := range current {
			if text, ok := raw.(string); ok {
				b.WriteString(text)
				continue
			}
			part := object(raw)
			if text := str(part["text"]); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

func collectResponsesToolSpecs(root map[string]any) []responsesToolSpec {
	var specs []responsesToolSpec
	seenChat := map[string]int{}
	var scan func([]any, string)
	scan = func(tools []any, namespace string) {
		for _, raw := range tools {
			tool := object(raw)
			kind := strings.TrimSpace(str(tool["type"]))
			if kind == "namespace" {
				scan(list(tool["tools"]), strings.TrimSpace(str(tool["name"])))
				continue
			}
			if kind != "" && kind != "function" && kind != "custom" {
				continue
			}
			name := strings.TrimSpace(str(tool["name"]))
			if name == "" {
				name = strings.TrimSpace(str(object(tool["function"])["name"]))
			}
			if name == "" {
				continue
			}
			chatName := responsesChatToolName(namespace, name)
			if previous, exists := seenChat[chatName]; exists {
				if specs[previous].Identity.Name == name && specs[previous].Identity.Namespace == namespace {
					continue
				}
				base := chatName
				for suffix := 1; ; suffix++ {
					candidate := capResponsesToolName(base + "_" + fmt.Sprint(suffix))
					if _, taken := seenChat[candidate]; !taken {
						chatName = candidate
						break
					}
				}
			}
			seenChat[chatName] = len(specs)
			specs = append(specs, responsesToolSpec{Identity: responsesToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom"}, Tool: tool, ChatName: chatName})
		}
	}
	scan(list(root["tools"]), "")
	for _, raw := range list(root["input"]) {
		item := object(raw)
		if str(item["type"]) == "additional_tools" {
			scan(list(item["tools"]), "")
		}
	}
	return specs
}

func capResponsesToolName(name string) string {
	if len(name) <= 64 {
		return name
	}
	name = name[len(name)-64:]
	if trimmed := strings.TrimLeft(name, "_-"); trimmed != "" {
		return trimmed
	}
	return name
}

func responsesToolSpecToChat(spec responsesToolSpec) map[string]any {
	function := map[string]any{"name": spec.ChatName, "description": "", "parameters": map[string]any{}}
	description := str(spec.Tool["description"])
	if description == "" {
		description = str(object(spec.Tool["function"])["description"])
	}
	if description != "" {
		function["description"] = description
	}
	if spec.Identity.Custom {
		function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []any{"input"}}
	} else {
		for _, key := range []string{"parameters", "parametersJsonSchema", "input_schema"} {
			if value, ok := spec.Tool[key]; ok {
				function["parameters"] = value
				break
			}
		}
		if nested := object(spec.Tool["function"]); nested != nil {
			for _, key := range []string{"parameters", "parametersJsonSchema"} {
				if value, ok := nested[key]; ok {
					function["parameters"] = value
					break
				}
			}
		}
	}
	return map[string]any{"type": "function", "function": function}
}

func responsesRequestToolChatName(root map[string]any, namespace, name string) string {
	for _, spec := range collectResponsesToolSpecs(root) {
		if spec.Identity.Name == name && spec.Identity.Namespace == namespace {
			return spec.ChatName
		}
	}
	return responsesChatToolName(namespace, name)
}

func responsesToolChoiceToChat(root map[string]any, value any) any {
	choice := object(value)
	if choice == nil {
		return value
	}
	kind := str(choice["type"])
	if kind != "function" && kind != "custom" {
		return value
	}
	name := str(object(choice["function"])["name"])
	if name == "" {
		name = str(object(choice["custom"])["name"])
	}
	if name == "" {
		name = str(choice["name"])
	}
	namespace := str(choice["namespace"])
	if namespace == "" {
		namespace = str(object(choice["function"])["namespace"])
	}
	if namespace == "" {
		namespace = str(object(choice["custom"])["namespace"])
	}
	if name == "" {
		return value
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": responsesRequestToolChatName(root, namespace, name)}}
}

func responsesTextFormatToChat(format map[string]any) map[string]any {
	kind := str(format["type"])
	switch kind {
	case "text", "json_object":
		return map[string]any{"type": kind}
	case "json_schema":
		schema := map[string]any{}
		for _, key := range []string{"name", "description", "strict", "schema"} {
			if value, ok := format[key]; ok {
				schema[key] = value
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}
	default:
		return nil
	}
}

func auditResponsesNativeInput(r ExecutorRequest) (int64, bool, string) {
	if str(r.Metadata["request_path"]) != "/v1/responses" || len(r.OriginalRequest) == 0 || len(r.Payload) == 0 {
		return 0, false, ""
	}
	var actual map[string]any
	if json.Unmarshal(r.Payload, &actual) != nil || actual == nil {
		return 0, false, "actual-invalid"
	}
	model := str(actual["model"])
	started := time.Now()
	candidate, err := responsesRequestToChatNative(r.OriginalRequest, model, r.Stream)
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		return elapsed, false, "candidate-error"
	}
	if reflect.DeepEqual(actual, candidate) {
		return elapsed, true, ""
	}
	return elapsed, false, firstResponsesDiff(actual, candidate, "$")
}

func firstResponsesDiff(actual, candidate any, path string) string {
	if reflect.DeepEqual(actual, candidate) {
		return ""
	}
	switch a := actual.(type) {
	case map[string]any:
		b, ok := candidate.(map[string]any)
		if !ok {
			return path + ":type"
		}
		for key, av := range a {
			bv, exists := b[key]
			if !exists {
				return path + "." + key + ":missing-candidate"
			}
			if diff := firstResponsesDiff(av, bv, path+"."+key); diff != "" {
				return diff
			}
		}
		for key := range b {
			if _, exists := a[key]; !exists {
				return path + "." + key + ":extra-candidate"
			}
		}
		return path + ":value"
	case []any:
		b, ok := candidate.([]any)
		if !ok {
			return path + ":type"
		}
		if len(a) != len(b) {
			return fmt.Sprintf("%s:length:%d:%d", path, len(a), len(b))
		}
		for i := range a {
			if diff := firstResponsesDiff(a[i], b[i], fmt.Sprintf("%s[%d]", path, i)); diff != "" {
				return diff
			}
		}
		return path + ":value"
	default:
		return path + ":value"
	}
}
