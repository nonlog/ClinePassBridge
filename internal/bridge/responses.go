package bridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type responsesToolIdentity struct {
	Name      string
	Namespace string
	Custom    bool
}

type responsesToolState struct {
	ID          string
	Name        string
	Identity    responsesToolIdentity
	Arguments   strings.Builder
	OutputIndex int
	Started     bool
	Done        bool
}

type responsesMessageState struct {
	OutputIndex int
	Text        strings.Builder
	Started     bool
	Done        bool
}

type responsesReasoningState struct {
	ID          string
	OutputIndex int
	Text        strings.Builder
	Started     bool
	Done        bool
}

type responsesStreamConverter struct {
	model            string
	request          map[string]any
	toolByChat       map[string]responsesToolIdentity
	toolByLocal      map[string]responsesToolIdentity
	seq              int
	responseID       string
	created          int64
	started          bool
	completed        bool
	nextOutputIndex  int
	messages         map[int]*responsesMessageState
	reasoning        *responsesReasoningState
	tools            map[string]*responsesToolState
	finishReason     string
	promptTokens     int64
	cachedTokens     int64
	completionTokens int64
	reasoningTokens  int64
	totalTokens      int64
	usageSeen        bool
}

func responsesOutputRequested(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai-response", "openai-responses", "responses":
		return true
	default:
		return false
	}
}

func newResponsesStreamConverter(model string, originalRequest, translatedRequest []byte) *responsesStreamConverter {
	var request map[string]any
	_ = json.Unmarshal(originalRequest, &request)
	if request == nil {
		request = map[string]any{}
	}
	byChat, byLocal := buildResponsesToolMap(originalRequest, translatedRequest)
	return &responsesStreamConverter{
		model:       model,
		request:     request,
		toolByChat:  byChat,
		toolByLocal: byLocal,
		messages:    map[int]*responsesMessageState{},
		tools:       map[string]*responsesToolState{},
	}
}

func responsesSSEEvent(event string, data map[string]any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(event)+len(body)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out, nil
}

func appendResponsesEvent(out *[][]byte, event string, data map[string]any) error {
	payload, err := responsesSSEEvent(event, data)
	if err != nil {
		return err
	}
	*out = append(*out, payload)
	return nil
}

func (c *responsesStreamConverter) nextSeq() int {
	c.seq++
	return c.seq
}

func (c *responsesStreamConverter) nextOutput() int {
	index := c.nextOutputIndex
	c.nextOutputIndex++
	return index
}

func (c *responsesStreamConverter) requestModel() string {
	if model := strings.TrimSpace(str(c.request["model"])); model != "" {
		return model
	}
	return c.model
}

func (c *responsesStreamConverter) ensureStarted(root map[string]any, out *[][]byte) error {
	if c.started {
		return nil
	}
	c.responseID = str(root["id"])
	if c.responseID == "" {
		c.responseID = "resp_" + id()
	}
	c.created = number(root["created"])
	if c.created == 0 {
		c.created = time.Now().Unix()
	}
	base := map[string]any{
		"id": c.responseID, "object": "response", "created_at": c.created,
		"status": "in_progress", "background": false, "error": nil, "output": []any{},
	}
	if model := c.requestModel(); model != "" {
		base["model"] = model
	}
	if err := appendResponsesEvent(out, "response.created", map[string]any{
		"type": "response.created", "sequence_number": c.nextSeq(), "response": cloneMap(base),
	}); err != nil {
		return err
	}
	if err := appendResponsesEvent(out, "response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": c.nextSeq(), "response": cloneMap(base),
	}); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *responsesStreamConverter) updateUsage(value any) {
	usage := object(value)
	if usage == nil {
		return
	}
	if _, ok := usage["prompt_tokens"]; ok {
		c.promptTokens = number(usage["prompt_tokens"])
		c.usageSeen = true
	}
	if details := object(usage["prompt_tokens_details"]); details != nil {
		if _, ok := details["cached_tokens"]; ok {
			c.cachedTokens = number(details["cached_tokens"])
			c.usageSeen = true
		}
	}
	if _, ok := usage["completion_tokens"]; ok {
		c.completionTokens = number(usage["completion_tokens"])
		c.usageSeen = true
	} else if _, ok := usage["output_tokens"]; ok {
		c.completionTokens = number(usage["output_tokens"])
		c.usageSeen = true
	}
	if details := object(usage["completion_tokens_details"]); details != nil {
		if _, ok := details["reasoning_tokens"]; ok {
			c.reasoningTokens = number(details["reasoning_tokens"])
			c.usageSeen = true
		}
	}
	if details := object(usage["output_tokens_details"]); details != nil {
		if _, ok := details["reasoning_tokens"]; ok {
			c.reasoningTokens = number(details["reasoning_tokens"])
			c.usageSeen = true
		}
	}
	if _, ok := usage["total_tokens"]; ok {
		c.totalTokens = number(usage["total_tokens"])
		c.usageSeen = true
	}
}

func responsesReasoningText(delta map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text := str(delta[key]); text != "" {
			return text
		}
	}
	return ""
}

func (c *responsesStreamConverter) ensureReasoning(choiceIndex int, out *[][]byte) error {
	if c.reasoning != nil && c.reasoning.Started && !c.reasoning.Done {
		return nil
	}
	r := &responsesReasoningState{ID: fmt.Sprintf("rs_%s_%d", c.responseID, choiceIndex), OutputIndex: c.nextOutput(), Started: true}
	c.reasoning = r
	if err := appendResponsesEvent(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": r.OutputIndex,
		"item": map[string]any{"id": r.ID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
	}); err != nil {
		return err
	}
	return appendResponsesEvent(out, "response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "sequence_number": c.nextSeq(),
		"item_id": r.ID, "output_index": r.OutputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (c *responsesStreamConverter) closeReasoning(out *[][]byte) error {
	r := c.reasoning
	if r == nil || !r.Started || r.Done {
		return nil
	}
	text := r.Text.String()
	if err := appendResponsesEvent(out, "response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "sequence_number": c.nextSeq(),
		"item_id": r.ID, "output_index": r.OutputIndex, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := appendResponsesEvent(out, "response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "sequence_number": c.nextSeq(),
		"item_id": r.ID, "output_index": r.OutputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	}); err != nil {
		return err
	}
	if err := appendResponsesEvent(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": r.OutputIndex,
		"item": map[string]any{"id": r.ID, "type": "reasoning", "encrypted_content": "", "summary": []any{map[string]any{"type": "summary_text", "text": text}}},
	}); err != nil {
		return err
	}
	r.Done = true
	return nil
}

func (c *responsesStreamConverter) ensureMessage(index int, out *[][]byte) (*responsesMessageState, error) {
	state := c.messages[index]
	if state == nil {
		state = &responsesMessageState{OutputIndex: c.nextOutput()}
		c.messages[index] = state
	}
	if state.Started {
		return state, nil
	}
	messageID := fmt.Sprintf("msg_%s_%d", c.responseID, index)
	if err := appendResponsesEvent(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
	}); err != nil {
		return nil, err
	}
	if err := appendResponsesEvent(out, "response.content_part.added", map[string]any{
		"type": "response.content_part.added", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
	}); err != nil {
		return nil, err
	}
	state.Started = true
	return state, nil
}

func responsesIncomplete(finishReason string) (string, map[string]any) {
	switch finishReason {
	case "length", "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func (c *responsesStreamConverter) closeMessage(index int, out *[][]byte) error {
	state := c.messages[index]
	if state == nil || !state.Started || state.Done {
		return nil
	}
	messageID := fmt.Sprintf("msg_%s_%d", c.responseID, index)
	text := state.Text.String()
	if err := appendResponsesEvent(out, "response.output_text.done", map[string]any{
		"type": "response.output_text.done", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := appendResponsesEvent(out, "response.content_part.done", map[string]any{
		"type": "response.content_part.done", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text},
	}); err != nil {
		return err
	}
	status, _ := responsesIncomplete(c.finishReason)
	if err := appendResponsesEvent(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{"id": messageID, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text}}},
	}); err != nil {
		return err
	}
	state.Done = true
	return nil
}

func responsesToolKey(choiceIndex, toolIndex int) string { return fmt.Sprintf("%d:%d", choiceIndex, toolIndex) }

func (c *responsesStreamConverter) identityForTool(name string) responsesToolIdentity {
	if identity, ok := c.toolByChat[name]; ok {
		return identity
	}
	if identity, ok := c.toolByLocal[name]; ok {
		return identity
	}
	return responsesToolIdentity{Name: name}
}

func (c *responsesStreamConverter) ensureTool(state *responsesToolState, out *[][]byte) error {
	if state.Started {
		return nil
	}
	if state.ID == "" && state.Name == "" {
		return nil
	}
	if state.ID == "" {
		state.ID = "call_" + id()
	}
	state.Identity = c.identityForTool(state.Name)
	name := state.Identity.Name
	if name == "" {
		name = state.Name
	}
	itemID, itemType := "fc_"+state.ID, "function_call"
	item := map[string]any{"id": itemID, "type": itemType, "status": "in_progress", "arguments": "", "call_id": state.ID, "name": name}
	if state.Identity.Custom {
		itemID, itemType = "ctc_"+state.ID, "custom_tool_call"
		item = map[string]any{"id": itemID, "type": itemType, "status": "in_progress", "input": "", "call_id": state.ID, "name": name}
	}
	if state.Identity.Namespace != "" {
		item["namespace"] = state.Identity.Namespace
	}
	if err := appendResponsesEvent(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
	}); err != nil {
		return err
	}
	state.Started = true
	return nil
}

func unwrapResponsesCustomInput(arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if input, ok := value["input"]; ok {
			if text, ok := input.(string); ok {
				return text
			}
			if encoded, err := json.Marshal(input); err == nil {
				return string(encoded)
			}
		}
	}
	return arguments
}

func (c *responsesStreamConverter) closeTool(state *responsesToolState, out *[][]byte) error {
	if state.Done {
		return nil
	}
	if err := c.ensureTool(state, out); err != nil {
		return err
	}
	if !state.Started {
		return nil
	}
	args := state.Arguments.String()
	if args == "" {
		args = "{}"
	}
	status, _ := responsesIncomplete(c.finishReason)
	name := state.Identity.Name
	if name == "" {
		name = state.Name
	}
	if state.Identity.Custom {
		input := unwrapResponsesCustomInput(args)
		if err := appendResponsesEvent(out, "response.custom_tool_call_input.done", map[string]any{
			"type": "response.custom_tool_call_input.done", "sequence_number": c.nextSeq(),
			"item_id": "ctc_" + state.ID, "output_index": state.OutputIndex, "input": input,
		}); err != nil {
			return err
		}
		item := map[string]any{"id": "ctc_" + state.ID, "type": "custom_tool_call", "status": status, "input": input, "call_id": state.ID, "name": name}
		if state.Identity.Namespace != "" {
			item["namespace"] = state.Identity.Namespace
		}
		if err := appendResponsesEvent(out, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
		}); err != nil {
			return err
		}
		state.Done = true
		return nil
	}
	if err := appendResponsesEvent(out, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "sequence_number": c.nextSeq(),
		"item_id": "fc_" + state.ID, "output_index": state.OutputIndex, "arguments": args,
	}); err != nil {
		return err
	}
	item := map[string]any{"id": "fc_" + state.ID, "type": "function_call", "status": status, "arguments": args, "call_id": state.ID, "name": name}
	if state.Identity.Namespace != "" {
		item["namespace"] = state.Identity.Namespace
	}
	if err := appendResponsesEvent(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
	}); err != nil {
		return err
	}
	state.Done = true
	return nil
}

func (c *responsesStreamConverter) finalizeOpenItems(out *[][]byte) error {
	if err := c.closeReasoning(out); err != nil {
		return err
	}
	indexes := make([]int, 0, len(c.messages))
	for index := range c.messages {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if err := c.closeMessage(index, out); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(c.tools))
	for key := range c.tools {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return c.tools[keys[i]].OutputIndex < c.tools[keys[j]].OutputIndex })
	for _, key := range keys {
		if err := c.closeTool(c.tools[key], out); err != nil {
			return err
		}
	}
	return nil
}

func (c *responsesStreamConverter) Feed(raw []byte) ([][]byte, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	c.updateUsage(root["usage"])
	choices := list(root["choices"])
	if len(choices) == 0 {
		return nil, nil
	}
	var out [][]byte
	if err := c.ensureStarted(root, &out); err != nil {
		return nil, err
	}
	for choicePosition, rawChoice := range choices {
		choice := object(rawChoice)
		index := int(number(choice["index"]))
		if _, exists := choice["index"]; !exists {
			index = choicePosition
		}
		delta := object(choice["delta"])
		if delta != nil {
			if reasoning := responsesReasoningText(delta); reasoning != "" {
				if err := c.ensureReasoning(index, &out); err != nil {
					return nil, err
				}
				c.reasoning.Text.WriteString(reasoning)
				if err := appendResponsesEvent(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "sequence_number": c.nextSeq(),
					"item_id": c.reasoning.ID, "output_index": c.reasoning.OutputIndex, "summary_index": 0, "delta": reasoning,
				}); err != nil {
					return nil, err
				}
			}
			if content := str(delta["content"]); content != "" {
				if err := c.closeReasoning(&out); err != nil {
					return nil, err
				}
				message, err := c.ensureMessage(index, &out)
				if err != nil {
					return nil, err
				}
				message.Text.WriteString(content)
				if err := appendResponsesEvent(&out, "response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "sequence_number": c.nextSeq(),
					"item_id": fmt.Sprintf("msg_%s_%d", c.responseID, index), "output_index": message.OutputIndex,
					"content_index": 0, "delta": content, "logprobs": []any{},
				}); err != nil {
					return nil, err
				}
			}
			for toolPosition, rawTool := range list(delta["tool_calls"]) {
				if err := c.closeReasoning(&out); err != nil {
					return nil, err
				}
				if err := c.closeMessage(index, &out); err != nil {
					return nil, err
				}
				tool := object(rawTool)
				toolIndex := int(number(tool["index"]))
				if _, exists := tool["index"]; !exists {
					toolIndex = toolPosition
				}
				key := responsesToolKey(index, toolIndex)
				state := c.tools[key]
				if state == nil {
					state = &responsesToolState{OutputIndex: c.nextOutput()}
					c.tools[key] = state
				}
				if callID := str(tool["id"]); callID != "" {
					state.ID = callID
				}
				function := object(tool["function"])
				if name := str(function["name"]); name != "" {
					state.Name = name
				}
				argsDelta := str(function["arguments"])
				if argsDelta != "" {
					state.Arguments.WriteString(argsDelta)
				}
				if err := c.ensureTool(state, &out); err != nil {
					return nil, err
				}
				if argsDelta != "" && state.Started && !state.Identity.Custom {
					if err := appendResponsesEvent(&out, "response.function_call_arguments.delta", map[string]any{
						"type": "response.function_call_arguments.delta", "sequence_number": c.nextSeq(),
						"item_id": "fc_" + state.ID, "output_index": state.OutputIndex, "delta": argsDelta,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
		if finish := str(choice["finish_reason"]); finish != "" {
			c.finishReason = finish
			if err := c.finalizeOpenItems(&out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (c *responsesStreamConverter) usage() map[string]any {
	if !c.usageSeen {
		return nil
	}
	total := c.totalTokens
	if total == 0 {
		total = c.promptTokens + c.completionTokens
	}
	outputDetails := map[string]any{}
	if c.reasoningTokens > 0 {
		outputDetails["reasoning_tokens"] = c.reasoningTokens
	}
	return map[string]any{
		"input_tokens": c.promptTokens,
		"input_tokens_details": map[string]any{"cached_tokens": c.cachedTokens},
		"output_tokens": c.completionTokens,
		"output_tokens_details": outputDetails,
		"total_tokens": total,
	}
}

func (c *responsesStreamConverter) completedOutput() []any {
	type indexed struct {
		index int
		item  map[string]any
	}
	items := make([]indexed, 0, len(c.messages)+len(c.tools)+1)
	if r := c.reasoning; r != nil && r.Started {
		items = append(items, indexed{r.OutputIndex, map[string]any{"id": r.ID, "type": "reasoning", "encrypted_content": "", "summary": []any{map[string]any{"type": "summary_text", "text": r.Text.String()}}}})
	}
	status, _ := responsesIncomplete(c.finishReason)
	for index, state := range c.messages {
		if !state.Started {
			continue
		}
		items = append(items, indexed{state.OutputIndex, map[string]any{
			"id": fmt.Sprintf("msg_%s_%d", c.responseID, index), "type": "message", "status": status, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": state.Text.String()}},
		}})
	}
	for _, state := range c.tools {
		if !state.Started {
			continue
		}
		name := state.Identity.Name
		if name == "" {
			name = state.Name
		}
		args := state.Arguments.String()
		if args == "" {
			args = "{}"
		}
		var item map[string]any
		if state.Identity.Custom {
			item = map[string]any{"id": "ctc_" + state.ID, "type": "custom_tool_call", "status": status, "input": unwrapResponsesCustomInput(args), "call_id": state.ID, "name": name}
		} else {
			item = map[string]any{"id": "fc_" + state.ID, "type": "function_call", "status": status, "arguments": args, "call_id": state.ID, "name": name}
		}
		if state.Identity.Namespace != "" {
			item["namespace"] = state.Identity.Namespace
		}
		items = append(items, indexed{state.OutputIndex, item})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func (c *responsesStreamConverter) completedResponse() map[string]any {
	status, incomplete := responsesIncomplete(c.finishReason)
	response := map[string]any{
		"id": c.responseID, "object": "response", "created_at": c.created, "status": status,
		"background": false, "error": nil, "output": c.completedOutput(),
	}
	if incomplete != nil {
		response["incomplete_details"] = incomplete
	} else {
		response["incomplete_details"] = nil
	}
	if model := c.requestModel(); model != "" {
		response["model"] = model
	}
	for _, key := range []string{"instructions", "max_output_tokens", "max_tool_calls", "parallel_tool_calls", "previous_response_id", "prompt_cache_key", "reasoning", "safety_identifier", "service_tier", "store", "temperature", "text", "tool_choice", "tools", "top_logprobs", "top_p", "truncation", "user", "metadata"} {
		if value, ok := c.request[key]; ok {
			response[key] = value
		}
	}
	if usage := c.usage(); usage != nil {
		response["usage"] = usage
	}
	return response
}

func (c *responsesStreamConverter) Done() ([][]byte, error) {
	if c.completed {
		return nil, nil
	}
	var out [][]byte
	if !c.started {
		return nil, fail(502, "upstream stream ended before a response item")
	}
	if err := c.finalizeOpenItems(&out); err != nil {
		return nil, err
	}
	status, _ := responsesIncomplete(c.finishReason)
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	body := map[string]any{"type": event, "sequence_number": c.nextSeq(), "response": c.completedResponse()}
	if err := appendResponsesEvent(&out, event, body); err != nil {
		return nil, err
	}
	c.completed = true
	return out, nil
}

func openAICompletionToResponses(raw []byte, model string, originalRequest, translatedRequest []byte) ([]byte, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	converter := newResponsesStreamConverter(model, originalRequest, translatedRequest)
	converter.responseID = str(root["id"])
	if converter.responseID == "" {
		converter.responseID = "resp_" + id()
	}
	converter.created = number(root["created"])
	if converter.created == 0 {
		converter.created = time.Now().Unix()
	}
	converter.started = true
	converter.updateUsage(root["usage"])
	for choicePosition, rawChoice := range list(root["choices"]) {
		choice := object(rawChoice)
		index := int(number(choice["index"]))
		if _, exists := choice["index"]; !exists {
			index = choicePosition
		}
		message := object(choice["message"])
		if reasoning := responsesReasoningText(message); reasoning != "" {
			converter.reasoning = &responsesReasoningState{ID: fmt.Sprintf("rs_%s_%d", converter.responseID, index), OutputIndex: converter.nextOutput(), Started: true, Done: true}
			converter.reasoning.Text.WriteString(reasoning)
		}
		if content := str(message["content"]); content != "" {
			state := &responsesMessageState{OutputIndex: converter.nextOutput(), Started: true, Done: true}
			state.Text.WriteString(content)
			converter.messages[index] = state
		}
		for toolPosition, rawTool := range list(message["tool_calls"]) {
			tool := object(rawTool)
			toolIndex := int(number(tool["index"]))
			if _, exists := tool["index"]; !exists {
				toolIndex = toolPosition
			}
			function := object(tool["function"])
			name := str(function["name"])
			state := &responsesToolState{ID: str(tool["id"]), Name: name, Identity: converter.identityForTool(name), OutputIndex: converter.nextOutput(), Started: true, Done: true}
			if state.ID == "" {
				state.ID = "call_" + id()
			}
			state.Arguments.WriteString(str(function["arguments"]))
			converter.tools[responsesToolKey(index, toolIndex)] = state
		}
		if finish := str(choice["finish_reason"]); finish != "" {
			converter.finishReason = finish
		}
	}
	return json.Marshal(converter.completedResponse())
}

func buildResponsesToolMap(originalRequest, translatedRequest []byte) (map[string]responsesToolIdentity, map[string]responsesToolIdentity) {
	declarations := collectResponsesToolDeclarations(originalRequest)
	var translated map[string]any
	_ = json.Unmarshal(translatedRequest, &translated)
	chatNames := make([]string, 0)
	for _, rawTool := range list(translated["tools"]) {
		name := str(object(object(rawTool)["function"])["name"])
		if name != "" {
			chatNames = append(chatNames, name)
		}
	}
	byChat := map[string]responsesToolIdentity{}
	byLocal := map[string]responsesToolIdentity{}
	ambiguousLocal := map[string]bool{}
	if len(chatNames) == len(declarations) {
		for i, name := range chatNames {
			byChat[name] = declarations[i]
		}
	} else {
		for _, declaration := range declarations {
			expected := responsesChatToolName(declaration.Namespace, declaration.Name)
			for _, chatName := range chatNames {
				if chatName == expected {
					byChat[chatName] = declaration
					break
				}
			}
		}
	}
	for _, declaration := range declarations {
		if existing, ok := byLocal[declaration.Name]; ok && (existing.Namespace != declaration.Namespace || existing.Custom != declaration.Custom) {
			ambiguousLocal[declaration.Name] = true
			continue
		}
		byLocal[declaration.Name] = declaration
	}
	for name := range ambiguousLocal {
		delete(byLocal, name)
	}
	return byChat, byLocal
}

func collectResponsesToolDeclarations(raw []byte) []responsesToolIdentity {
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	var out []responsesToolIdentity
	var scan func([]any, string)
	scan = func(tools []any, namespace string) {
		for _, rawTool := range tools {
			tool := object(rawTool)
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
			out = append(out, responsesToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom"})
		}
	}
	scan(list(root["tools"]), "")
	for _, rawItem := range list(root["input"]) {
		item := object(rawItem)
		if str(item["type"]) == "additional_tools" {
			scan(list(item["tools"]), "")
		}
	}
	return out
}

func responsesChatToolName(namespace, name string) string {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(namespace)
	if namespace != "" && !strings.HasPrefix(name, "mcp__") && !strings.HasPrefix(name, namespace) {
		if strings.HasSuffix(namespace, "__") {
			name = namespace + name
		} else {
			name = namespace + "__" + name
		}
	}
	if len(name) <= 64 {
		return name
	}
	name = name[len(name)-64:]
	if trimmed := strings.TrimLeft(name, "_-"); trimmed != "" {
		return trimmed
	}
	return name
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
