package bridge

import (
	"github.com/xiao-qiu-qiu/ClinePassBridge/internal/translator"
)

func (s *Service) convertClaudeInput(payload []byte, model string, stream bool) (map[string]any, error) {
	result, err := translator.ConvertClaudeToChatCompletions(model, payload, stream)
	if err != nil {
		return nil, fail(400, "Claude input conversion failed: "+err.Error())
	}
	return decodeObject(result)
}

func (s *Service) convertResponsesInput(payload []byte, model string, stream bool) (map[string]any, error) {
	result, err := translator.ConvertResponsesToChatCompletions(model, payload, stream)
	if err != nil {
		return nil, fail(400, "Responses input conversion failed: "+err.Error())
	}
	return decodeObject(result)
}
