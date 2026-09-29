package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	bridge "github.com/xiao-qiu-qiu/ClinePassBridge/internal/bridge"
)

type server struct {
	engine  *bridge.StandaloneEngine
	token   string
	maxBody int64
}

type preparedRequest struct {
	model          string
	stream         bool
	sourceFormat   string
	outputFormat   string
	payload        []byte
	affinity       string
	affinitySource string
	parseMS        int64
	translateMS    int64
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 24 {
		return "", errors.New("gateway token must contain at least 24 characters")
	}
	return token, nil
}

func bearer(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func (s *server) authorized(r *http.Request) bool {
	got := bearer(r)
	if got == "" || len(got) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func rawString(fields map[string]json.RawMessage, key string) string {
	raw := fields[key]
	if len(raw) == 0 {
		return ""
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value)
}

func rawBool(fields map[string]json.RawMessage, key string) bool {
	raw := fields[key]
	if len(raw) == 0 {
		return false
	}
	var value bool
	_ = json.Unmarshal(raw, &value)
	return value
}

func affinityFromRequest(r *http.Request, fields map[string]json.RawMessage, raw []byte) (string, string) {
	for _, name := range []string{
		"X-NewAPI-Affinity", "X-Session-Affinity",
		"Session-Id", "X-Session-Id", "X-Claude-Code-Session-Id",
		"X-Conversation-Id", "X-Thread-Id", "Thread-Id",
		"X-Codex-Thread-Id", "X-Codex-Parent-Thread-Id",
		"X-Parent-Session-Id", "X-Parent-Thread-Id",
		"X-Codex-Session-Id", "X-Client-Session-Id",
		"X-OpenAI-Session-Id", "X-Anthropic-Session-Id", "OpenAI-Conversation-Id",
	} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return "header:" + strings.ToLower(name) + ":" + value, "header:" + name
		}
	}

	for _, key := range []string{
		"execution_session_id", "prompt_cache_key", "session_id",
		"conversation_id", "thread_id", "parent_session_id", "parent_thread_id",
	} {
		if value := rawString(fields, key); value != "" {
			return "body:" + key + ":" + value, "body:" + key
		}
	}

	if rawExtra := fields["extra_body"]; len(rawExtra) > 0 {
		var extra map[string]json.RawMessage
		if json.Unmarshal(rawExtra, &extra) == nil {
			for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
				if value := rawString(extra, key); value != "" {
					return "extra_body:" + key + ":" + value, "extra_body:" + key
				}
			}
		}
	}

	if rawMeta := fields["metadata"]; len(rawMeta) > 0 {
		var meta map[string]json.RawMessage
		if json.Unmarshal(rawMeta, &meta) == nil {
			for _, key := range []string{"session_id", "conversation_id", "thread_id", "parent_session_id"} {
				if value := rawString(meta, key); value != "" {
					return "metadata:" + key + ":" + value, "metadata:" + key
				}
			}
			if rawUser := rawString(meta, "user_id"); strings.HasPrefix(rawUser, "{") {
				var user map[string]json.RawMessage
				if json.Unmarshal([]byte(rawUser), &user) == nil {
					for _, key := range []string{"session_id", "parent_session_id"} {
						if value := rawString(user, key); value != "" {
							return "metadata.user_id:" + key + ":" + value, "metadata.user_id:" + key
						}
					}
				}
			}
		}
	}

	return "", "round-robin"
}

func prepare(path string, r *http.Request, raw []byte) (preparedRequest, error) {
	parseStart := time.Now()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return preparedRequest{}, fmt.Errorf("invalid request JSON: %w", err)
	}
	model := rawString(fields, "model")
	if model == "" {
		return preparedRequest{}, errors.New("model is required")
	}
	stream := rawBool(fields, "stream")
	affinity, affinitySource := affinityFromRequest(r, fields, raw)
	parseMS := time.Since(parseStart).Milliseconds()

	var from, output sdktranslator.Format
	switch path {
	case "/v1/messages":
		from = sdktranslator.FormatClaude
		output = sdktranslator.FormatClaude
	case "/v1/responses":
		from = sdktranslator.FormatOpenAIResponse
		output = sdktranslator.FormatOpenAIResponse
	case "/v1/chat/completions":
		from = sdktranslator.FormatOpenAI
		output = sdktranslator.FormatOpenAI
	default:
		return preparedRequest{}, errors.New("unsupported path")
	}

	translateStart := time.Now()
	payload := raw
	if from != sdktranslator.FormatOpenAI {
		if !sdktranslator.HasRequestTransformerByFormatName(from, sdktranslator.FormatOpenAI) {
			return preparedRequest{}, fmt.Errorf("CPA v8.0.4 translator does not support %s to openai", from)
		}
		payload = sdktranslator.TranslateRequestByFormatName(from, sdktranslator.FormatOpenAI, model, raw, stream)
		if len(payload) == 0 || !json.Valid(payload) {
			return preparedRequest{}, fmt.Errorf("CPA v8.0.4 translator returned invalid %s request", from)
		}
	}
	translateMS := time.Since(translateStart).Milliseconds()

	outputFormat := "chat-completions"
	if output == sdktranslator.FormatClaude {
		outputFormat = "claude"
	} else if output == sdktranslator.FormatOpenAIResponse {
		outputFormat = "openai-response"
	}
	return preparedRequest{
		model:          model,
		stream:         stream,
		sourceFormat:   string(from),
		outputFormat:   outputFormat,
		payload:        payload,
		affinity:       affinity,
		affinitySource: affinitySource,
		parseMS:        parseMS,
		translateMS:    translateMS,
	}, nil
}

func errorStatus(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		code := status.StatusCode()
		if code >= 400 && code <= 599 {
			return code
		}
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusBadGateway
}

func writeError(w http.ResponseWriter, path string, err error) {
	status := errorStatus(err)
	if status == 499 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	message := err.Error()
	if path == "/v1/messages" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status := s.engine.Status()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":              "ok",
		"version":             status.Version,
		"enabled_credentials": status.EnabledCredentials,
		"model_count":         status.ModelCount,
	})
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "authentication_error", "message": "invalid gateway token"},
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r.URL.Path, fmt.Errorf("read request body: %w", err))
		return
	}
	prepared, err := prepare(r.URL.Path, r, raw)
	if err != nil {
		writeError(w, r.URL.Path, err)
		return
	}

	req := bridge.StandaloneRequest{
		Context:         r.Context(),
		Model:           prepared.model,
		OutputFormat:    prepared.outputFormat,
		SourceFormat:    prepared.sourceFormat,
		RequestPath:     r.URL.Path,
		AffinityKey:     prepared.affinity,
		OriginalRequest: raw,
		Payload:         prepared.payload,
		Headers:         r.Header,
		Metadata: map[string]any{
			"gateway_body_bytes":      int64(len(raw)),
			"gateway_parse_ms":        prepared.parseMS,
			"gateway_translate_ms":    prepared.translateMS,
			"gateway_affinity_source": prepared.affinitySource,
		},
	}

	if !prepared.stream {
		resp, execErr := s.engine.Execute(req)
		if execErr != nil {
			writeError(w, r.URL.Path, execErr)
			return
		}
		for key, values := range resp.Headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("X-ClinePass-Gateway", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.Payload)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r.URL.Path, errors.New("streaming is unavailable"))
		return
	}
	wrote := false
	writeStreamHeaders := func() {
		if wrote {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-ClinePass-Gateway", "1")
		w.WriteHeader(http.StatusOK)
		wrote = true
	}

	execErr := s.engine.ExecuteStream(req, func(payload []byte) error {
		writeStreamHeaders()
		var data []byte
		if r.URL.Path == "/v1/chat/completions" {
			data = make([]byte, 0, len(payload)+8)
			data = append(data, "data: "...)
			data = append(data, payload...)
			data = append(data, '\n', '\n')
		} else {
			data = payload
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if execErr != nil {
		if !wrote {
			writeError(w, r.URL.Path, execErr)
		}
		return
	}
	writeStreamHeaders()
	if r.URL.Path == "/v1/chat/completions" {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func main() {
	addr := env("CLINEPASS_GATEWAY_ADDR", "0.0.0.0:8320")
	settingsPath := env("CLINEPASS_GATEWAY_SETTINGS", "/control/settings.json")
	authDir := env("CLINEPASS_GATEWAY_AUTH_DIR", "/auths")
	dataDir := env("CLINEPASS_GATEWAY_DATA_DIR", "/data")
	tokenFile := env("CLINEPASS_GATEWAY_TOKEN_FILE", "/run/secrets/clinepass-gateway-token")
	token, err := readToken(tokenFile)
	if err != nil {
		log.Fatalf("load gateway token: %v", err)
	}
	engine, err := bridge.NewStandaloneEngine(bridge.StandaloneConfig{
		SettingsPath: settingsPath,
		AuthDir:      authDir,
		DataDir:      dataDir,
	})
	if err != nil {
		log.Fatalf("initialize gateway: %v", err)
	}
	s := &server{
		engine:  engine,
		token:   token,
		maxBody: envInt64("CLINEPASS_GATEWAY_MAX_BODY_BYTES", 128<<20),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/messages", s.proxy)
	mux.HandleFunc("/v1/responses", s.proxy)
	mux.HandleFunc("/v1/chat/completions", s.proxy)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("ClinePass Gateway listening on %s", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway server: %v", err)
	}
}
