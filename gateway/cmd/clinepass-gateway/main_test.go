package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrepareClaudeUsesCPATranslatorAndKeepsAffinity(t *testing.T) {
	raw := []byte("{\"model\":\"deepseek-v4.1-flash\",\"stream\":true,\"prompt_cache_key\":\"conversation-123\",\"system\":\"system prompt\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	got, err := prepare("/v1/messages", req, raw)
	if err != nil {
		t.Fatalf("prepare messages: %v", err)
	}
	if got.model != "deepseek-v4.1-flash" || !got.stream || got.sourceFormat != "claude" || got.outputFormat != "claude" {
		t.Fatalf("unexpected prepared messages request: %#v", got)
	}
	if got.affinitySource != "body:prompt_cache_key" {
		t.Fatalf("affinity source = %q", got.affinitySource)
	}
	var translated map[string]any
	if err := json.Unmarshal(got.payload, &translated); err != nil {
		t.Fatalf("translated payload is invalid JSON: %v", err)
	}
	if _, ok := translated["messages"]; !ok {
		t.Fatalf("translated Claude payload has no messages: %#v", translated)
	}
}

func TestPrepareResponsesUsesCPATranslator(t *testing.T) {
	raw := []byte("{\"model\":\"deepseek-v4.1-flash\",\"stream\":true,\"input\":\"hello\",\"tools\":[{\"type\":\"function\",\"name\":\"lookup\",\"description\":\"lookup\",\"parameters\":{\"type\":\"object\",\"properties\":{}}}]}")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	got, err := prepare("/v1/responses", req, raw)
	if err != nil {
		t.Fatalf("prepare responses: %v", err)
	}
	if got.sourceFormat != "openai-response" || got.outputFormat != "openai-response" {
		t.Fatalf("unexpected Responses formats: %#v", got)
	}
	if len(got.payload) == 0 || !json.Valid(got.payload) {
		t.Fatalf("translated Responses payload is invalid: %q", got.payload)
	}
}

func TestGatewayAuthorization(t *testing.T) {
	s := &server{token: "0123456789abcdef0123456789abcdef"}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
	if !s.authorized(req) {
		t.Fatal("valid bearer token was rejected")
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if s.authorized(req) {
		t.Fatal("invalid bearer token was accepted")
	}
}

func TestAffinityHeaderOverridesBody(t *testing.T) {
	raw := []byte("{\"prompt_cache_key\":\"body-value\"}")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Session-Affinity", "header-value")
	value, source := affinityFromRequest(req, fields, raw)
	if source != "header:X-Session-Affinity" || value != "header:x-session-affinity:header-value" {
		t.Fatalf("unexpected affinity value=%q source=%q", value, source)
	}
}
