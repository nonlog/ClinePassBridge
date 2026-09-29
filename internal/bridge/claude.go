package bridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func claudeOutputRequested(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic":
		return true
	default:
		return false
	}
}

func claudeCanonicalToolName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, "_")
	return strings.ToLower(name)
}

func claudeToolNameMap(original []byte) map[string]string {
	if len(original) == 0 {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(original, &root) != nil {
		return nil
	}
	out := map[string]string{}
	for _, value := range list(root["tools"]) {
		tool := object(value)
		name := strings.TrimSpace(str(tool["name"]))
		if name == "" {
			name = strings.TrimSpace(str(object(tool["function"])["name"]))
		}
		key := claudeCanonicalToolName(name)
		if key == "" {
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = name
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func claudeMapToolName(names map[string]string, name string) string {
	if mapped := names[claudeCanonicalToolName(name)]; mapped != "" {
		return mapped
	}
	return name
}

func claudeSanitizeToolID(value string) string {
	if value == "" {
		return "toolu_" + id()
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			b.WriteByte(ch)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "toolu_" + id()
	}
	return b.String()
}

func claudeSSEEvent(event string, data map[string]any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(event)+len(body)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, 10)
	out = append(out, "data: "...)
	out = append(out, body...)
	out = append(out, 10, 10)
	return out, nil
}

func appendClaudeEvent(out *[][]byte, event string, data map[string]any) error {
	payload, err := claudeSSEEvent(event, data)
	if err != nil {
		return err
	}
	*out = append(*out, payload)
	return nil
}

func claudeReasoningTexts(obj map[string]any) []string {
	for _, key := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
		var out []string
		collectClaudeReasoning(obj[key], &out)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func collectClaudeReasoning(value any, out *[]string) {
	switch current := value.(type) {
	case string:
		if current != "" {
			*out = append(*out, current)
		}
	case []any:
		for _, item := range current {
			collectClaudeReasoning(item, out)
		}
	case map[string]any:
		if text := str(current["text"]); text != "" {
			*out = append(*out, text)
		}
	}
}

func claudeUsageFromOpenAI(value any) map[string]any {
	usage := object(value)
	prompt := number(usage["prompt_tokens"])
	output := number(usage["completion_tokens"])
	details := object(usage["prompt_tokens_details"])
	cached := number(details["cached_tokens"])
	cacheWrite := number(details["cache_write_tokens"])
	if cacheWrite <= 0 {
		cacheWrite = number(details["cache_creation_tokens"])
	}
	input := prompt
	if cached > 0 {
		input -= cached
	}
	if cacheWrite > 0 {
		input -= cacheWrite
	}
	if input < 0 {
		input = 0
	}
	out := map[string]any{"input_tokens": input, "output_tokens": output}
	if cached > 0 {
		out["cache_read_input_tokens"] = cached
	}
	if cacheWrite > 0 {
		out["cache_creation_input_tokens"] = cacheWrite
	}
	return out
}

func claudeFinishReason(reason string, sawTool, invalidTool bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	}
	if invalidTool {
		return "max_tokens"
	}
	if sawTool {
		return "tool_use"
	}
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func claudeToolInput(arguments string) (map[string]any, string, bool) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}, "", true
	}
	var value any
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return map[string]any{}, "{}", false
	}
	input, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}, "{}", false
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return map[string]any{}, "{}", false
	}
	return input, string(normalized), true
}

type claudeToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

type claudeStreamConverter struct {
	model          string
	messageID      string
	toolNames      map[string]string
	started        bool
	nextBlock      int64
	openKind       string
	openBlock      int64
	tools           map[int64]*claudeToolCall
	finishReason    string
	sawTool         bool
	invalidTool     bool
	blocksFinalized bool
	terminalSent    bool
	usage           map[string]any
}

func newClaudeStreamConverter(model string, original []byte) *claudeStreamConverter {
	return &claudeStreamConverter{
		model:     model,
		toolNames: claudeToolNameMap(original),
		tools:     map[int64]*claudeToolCall{},
		usage:     map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
	}
}

func (c *claudeStreamConverter) ensureMessageStarted(root map[string]any, out *[][]byte) error {
	if c.started {
		return nil
	}
	c.messageID = str(root["id"])
	if c.messageID == "" {
		c.messageID = "msg_" + id()
	}
	message := map[string]any{
		"id":            c.messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         c.model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
	}
	if err := appendClaudeEvent(out, "message_start", map[string]any{"type": "message_start", "message": message}); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *claudeStreamConverter) closeOpenBlock(out *[][]byte) error {
	if c.openKind == "" {
		return nil
	}
	if err := appendClaudeEvent(out, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": c.openBlock,
	}); err != nil {
		return err
	}
	c.openKind = ""
	c.openBlock = -1
	return nil
}

func (c *claudeStreamConverter) ensureOpenBlock(kind string, out *[][]byte) error {
	if c.openKind == kind {
		return nil
	}
	if err := c.closeOpenBlock(out); err != nil {
		return err
	}
	index := c.nextBlock
	c.nextBlock++
	var block map[string]any
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": ""}
	} else {
		block = map[string]any{"type": "text", "text": ""}
	}
	if err := appendClaudeEvent(out, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": block,
	}); err != nil {
		return err
	}
	c.openKind = kind
	c.openBlock = index
	return nil
}

func (c *claudeStreamConverter) emitThinking(text string, out *[][]byte) error {
	if text == "" {
		return nil
	}
	if err := c.ensureOpenBlock("thinking", out); err != nil {
		return err
	}
	return appendClaudeEvent(out, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": c.openBlock,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (c *claudeStreamConverter) emitText(text string, out *[][]byte) error {
	if text == "" {
		return nil
	}
	if err := c.ensureOpenBlock("text", out); err != nil {
		return err
	}
	return appendClaudeEvent(out, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": c.openBlock,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (c *claudeStreamConverter) observeToolCalls(delta map[string]any) {
	for arrayIndex, value := range list(delta["tool_calls"]) {
		call := object(value)
		index := number(call["index"])
		if _, exists := call["index"]; !exists {
			index = int64(arrayIndex)
		}
		accumulator := c.tools[index]
		if accumulator == nil {
			accumulator = &claudeToolCall{}
			c.tools[index] = accumulator
		}
		if callID := str(call["id"]); callID != "" {
			accumulator.ID = callID
		}
		function := object(call["function"])
		if name := str(function["name"]); name != "" {
			accumulator.Name = name
		}
		if arguments := str(function["arguments"]); arguments != "" {
			accumulator.Arguments.WriteString(arguments)
		}
	}
}

func (c *claudeStreamConverter) finalizeBlocks(out *[][]byte) error {
	if c.blocksFinalized {
		return nil
	}
	if err := c.closeOpenBlock(out); err != nil {
		return err
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, int(index))
	}
	sort.Ints(indexes)
	for _, rawIndex := range indexes {
		tool := c.tools[int64(rawIndex)]
		if tool == nil || (tool.ID == "" && tool.Name == "" && tool.Arguments.Len() == 0) {
			continue
		}
		name := tool.Name
		if name == "" {
			name = fmt.Sprintf("tool_%d", rawIndex)
		}
		name = claudeMapToolName(c.toolNames, name)
		toolID := claudeSanitizeToolID(tool.ID)
		input, partial, valid := claudeToolInput(tool.Arguments.String())
		if !valid {
			c.invalidTool = true
		}
		index := c.nextBlock
		c.nextBlock++
		if err := appendClaudeEvent(out, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    toolID,
				"name":  name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		if partial != "" {
			if err := appendClaudeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
			}); err != nil {
				return err
			}
		}
		if err := appendClaudeEvent(out, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		}); err != nil {
			return err
		}
		_ = input
		c.sawTool = true
	}
	c.blocksFinalized = true
	return nil
}

func (c *claudeStreamConverter) emitTerminal(out *[][]byte) error {
	if c.terminalSent {
		return nil
	}
	if err := c.finalizeBlocks(out); err != nil {
		return err
	}
	usage := c.usage
	if usage == nil {
		usage = map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}
	}
	if err := appendClaudeEvent(out, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   claudeFinishReason(c.finishReason, c.sawTool, c.invalidTool),
			"stop_sequence": nil,
		},
		"usage": usage,
	}); err != nil {
		return err
	}
	if err := appendClaudeEvent(out, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	c.terminalSent = true
	return nil
}

func (c *claudeStreamConverter) Feed(raw []byte) ([][]byte, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	choices := list(root["choices"])
	if len(choices) > 0 {
		if err := c.ensureMessageStarted(root, &out); err != nil {
			return nil, err
		}
		choice := object(choices[0])
		delta := object(choice["delta"])
		for _, reasoning := range claudeReasoningTexts(delta) {
			if err := c.emitThinking(reasoning, &out); err != nil {
				return nil, err
			}
		}
		if content := str(delta["content"]); content != "" {
			if err := c.emitText(content, &out); err != nil {
				return nil, err
			}
		}
		if len(list(delta["tool_calls"])) > 0 {
			c.observeToolCalls(delta)
		}
		if reason := str(choice["finish_reason"]); reason != "" {
			c.finishReason = reason
			if err := c.finalizeBlocks(&out); err != nil {
				return nil, err
			}
		}
	}
	if usage := object(root["usage"]); usage != nil {
		c.usage = claudeUsageFromOpenAI(usage)
	}
	if c.finishReason != "" && object(root["usage"]) != nil {
		if err := c.emitTerminal(&out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *claudeStreamConverter) Done() ([][]byte, error) {
	var out [][]byte
	if !c.started {
		if err := c.ensureMessageStarted(map[string]any{}, &out); err != nil {
			return nil, err
		}
	}
	if err := c.emitTerminal(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func openAICompletionToClaude(body []byte, model string, original []byte) ([]byte, error) {
	root, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	names := claudeToolNameMap(original)
	content := []any{}
	sawTool := false
	invalidTool := false
	finishReason := ""
	choices := list(root["choices"])
	if len(choices) > 0 {
		choice := object(choices[0])
		finishReason = str(choice["finish_reason"])
		message := object(choice["message"])
		for _, reasoning := range claudeReasoningTexts(message) {
			if reasoning != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
			}
		}
		switch value := message["content"].(type) {
		case string:
			if value != "" {
				content = append(content, map[string]any{"type": "text", "text": value})
			}
		case []any:
			for _, item := range value {
				part := object(item)
				switch str(part["type"]) {
				case "text":
					if text := str(part["text"]); text != "" {
						content = append(content, map[string]any{"type": "text", "text": text})
					}
				case "thinking":
					if thinking := str(part["thinking"]); thinking != "" {
						content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
					}
				}
			}
		}
		for _, value := range list(message["tool_calls"]) {
			call := object(value)
			function := object(call["function"])
			name := claudeMapToolName(names, str(function["name"]))
			input, _, valid := claudeToolInput(str(function["arguments"]))
			if !valid {
				invalidTool = true
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    claudeSanitizeToolID(str(call["id"])),
				"name":  name,
				"input": input,
			})
			sawTool = true
		}
	}
	messageID := str(root["id"])
	if messageID == "" {
		messageID = "msg_" + id()
	}
	out := map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   claudeFinishReason(finishReason, sawTool, invalidTool),
		"stop_sequence": nil,
		"usage":         claudeUsageFromOpenAI(root["usage"]),
	}
	return json.Marshal(out)
}
