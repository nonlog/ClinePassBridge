package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type StandaloneConfig struct {
	SettingsPath string
	AuthDir      string
	DataDir      string
}

type StandaloneRequest struct {
	Context         context.Context
	Model           string
	OutputFormat    string
	SourceFormat    string
	RequestPath     string
	AffinityKey     string
	OriginalRequest []byte
	Payload         []byte
	Headers         http.Header
	Metadata        map[string]any
}

type StandaloneResponse struct {
	Payload []byte
	Headers http.Header
}

type StandaloneStatus struct {
	Version            string
	EnabledCredentials int
	ModelCount         int
}

type standaloneStreamEvent struct {
	payload []byte
	errText string
	done    bool
}

type standaloneStreamSink struct {
	ctx    context.Context
	cancel context.CancelFunc
	events chan standaloneStreamEvent
	once   sync.Once
}

func (s *standaloneStreamSink) finish(errText string) {
	s.once.Do(func() {
		select {
		case s.events <- standaloneStreamEvent{errText: errText, done: true}:
		case <-s.ctx.Done():
		}
		close(s.events)
	})
}

type StandaloneEngine struct {
	service *Service
	cfg     StandaloneConfig
	sinksMu sync.RWMutex
	sinks   map[string]*standaloneStreamSink
	rr      atomic.Uint64
}

func NewStandaloneEngine(cfg StandaloneConfig) (*StandaloneEngine, error) {
	cfg.SettingsPath = strings.TrimSpace(cfg.SettingsPath)
	cfg.AuthDir = strings.TrimSpace(cfg.AuthDir)
	cfg.DataDir = strings.TrimSpace(cfg.DataDir)
	if cfg.SettingsPath == "" || cfg.AuthDir == "" || cfg.DataDir == "" {
		return nil, errors.New("settings path, auth dir, and data dir are required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("create gateway data dir: %w", err)
	}
	e := &StandaloneEngine{
		service: NewService(),
		cfg:     cfg,
		sinks:   map[string]*standaloneStreamSink{},
	}
	e.service.SetHost(e.hostCall)
	if err := e.reloadControlPlane(); err != nil {
		return nil, err
	}
	if err := e.reloadCredentials(); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "requests.json")); err == nil {
		var logs []LogEntry
		if json.Unmarshal(b, &logs) == nil {
			e.service.mu.Lock()
			e.service.logs = logs
			if limit := e.service.cfg.LogRetention; limit > 0 && len(e.service.logs) > limit {
				e.service.logs = e.service.logs[len(e.service.logs)-limit:]
			}
			e.service.mu.Unlock()
		}
	}
	return e, nil
}

func (e *StandaloneEngine) reloadControlPlane() error {
	b, err := os.ReadFile(e.cfg.SettingsPath)
	if err != nil {
		return fmt.Errorf("read control-plane settings: %w", err)
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("decode control-plane settings: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	cfg.TransportMode = "direct"
	cfg.DataDir = e.cfg.DataDir
	e.service.mu.Lock()
	e.service.cfg = cfg
	e.service.mu.Unlock()
	return nil
}

func (e *StandaloneEngine) reloadCredentials() error {
	entries, err := os.ReadDir(e.cfg.AuthDir)
	if err != nil {
		return fmt.Errorf("read auth dir: %w", err)
	}
	creds := make(map[string]Credential)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(e.cfg.AuthDir, entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var c Credential
		if json.Unmarshal(b, &c) != nil || c.Type != Provider || strings.TrimSpace(c.APIKey) == "" {
			continue
		}
		if strings.TrimSpace(c.ID) == "" {
			c.ID = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		if strings.TrimSpace(c.Label) == "" {
			c.Label = c.ID
		}
		c.RequestScopedErrors = requestErrorRules()
		creds[c.ID] = c
	}
	if len(creds) == 0 {
		return errors.New("no Cline Pass credentials are available in the auth store")
	}
	e.service.mu.Lock()
	e.service.creds = creds
	e.service.mu.Unlock()
	return nil
}

func (e *StandaloneEngine) Status() StandaloneStatus {
	_ = e.reloadControlPlane()
	_ = e.reloadCredentials()
	e.service.mu.RLock()
	defer e.service.mu.RUnlock()
	enabled := 0
	for _, c := range e.service.creds {
		if !c.Disabled && c.APIKey != "" {
			enabled++
		}
	}
	return StandaloneStatus{
		Version:            Version,
		EnabledCredentials: enabled,
		ModelCount:         len(e.service.cfg.Models),
	}
}

func (e *StandaloneEngine) orderedCredentials(affinity string) ([]Credential, error) {
	if err := e.reloadControlPlane(); err != nil {
		return nil, err
	}
	if err := e.reloadCredentials(); err != nil {
		return nil, err
	}
	e.service.mu.RLock()
	creds := make([]Credential, 0, len(e.service.creds))
	for _, c := range e.service.creds {
		if !c.Disabled && c.APIKey != "" {
			creds = append(creds, c)
		}
	}
	e.service.mu.RUnlock()
	if len(creds) == 0 {
		return nil, fail(503, "no enabled Cline Pass credentials are available")
	}
	sort.Slice(creds, func(i, j int) bool { return creds[i].ID < creds[j].ID })
	var start uint64
	if affinity = strings.TrimSpace(affinity); affinity != "" {
		sum := sha256.Sum256([]byte(affinity))
		start = binary.BigEndian.Uint64(sum[:8])
	} else {
		start = e.rr.Add(1) - 1
	}
	start %= uint64(len(creds))
	ordered := make([]Credential, 0, len(creds))
	for i := 0; i < len(creds); i++ {
		ordered = append(ordered, creds[(int(start)+i)%len(creds)])
	}
	return ordered, nil
}

func (e *StandaloneEngine) executorRequest(req StandaloneRequest, c Credential) ExecutorRequest {
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	metadata := map[string]any{"request_path": req.RequestPath, "gateway": true}
	for k, v := range req.Metadata {
		metadata[k] = v
	}
	return ExecutorRequest{
		ctx:             ctx,
		AuthID:          c.ID,
		AuthProvider:    Provider,
		Model:           req.Model,
		Format:          req.OutputFormat,
		SourceFormat:    req.SourceFormat,
		Headers:         req.Headers.Clone(),
		OriginalRequest: append([]byte(nil), req.OriginalRequest...),
		Payload:         append([]byte(nil), req.Payload...),
		StorageJSON:     jsonBytes(c),
		Metadata:        metadata,
	}
}

func standaloneRetryable(err error, c Credential) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, rule := range c.RequestScopedErrors {
		if rule.Action != "stop" {
			continue
		}
		for _, match := range rule.Match {
			if strings.Contains(msg, strings.ToLower(match)) {
				return false
			}
		}
	}
	switch statusOf(err) {
	case 401, 403, 408, 409, 425, 429, 500, 502, 503:
		return true
	default:
		return false
	}
}

func standaloneRetryableText(errText string) bool {
	s := strings.ToLower(errText)
	if s == "" || strings.Contains(s, "timed out") || strings.Contains(s, "empty response content") {
		return false
	}
	for _, marker := range []string{
		"unauthorized", "forbidden", "rate limit", "quota", "no available",
		"temporarily unavailable", "upstream transport failed", "status 401",
		"status 403", "status 429", "status 500", "status 502", "status 503",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func (e *StandaloneEngine) Execute(req StandaloneRequest) (StandaloneResponse, error) {
	credentials, err := e.orderedCredentials(req.AffinityKey)
	if err != nil {
		return StandaloneResponse{}, err
	}
	var last error
	for i, c := range credentials {
		r := e.executorRequest(req, c)
		out, execErr := e.service.execute(r)
		if execErr == nil {
			resp, ok := out.(Response)
			if !ok {
				return StandaloneResponse{}, fail(500, "unexpected standalone response type")
			}
			return StandaloneResponse{Payload: resp.Payload, Headers: resp.Headers}, nil
		}
		last = execErr
		if i == len(credentials)-1 || !standaloneRetryable(execErr, c) {
			break
		}
	}
	if last == nil {
		last = fail(503, "no Cline Pass credential completed the request")
	}
	return StandaloneResponse{}, last
}

func (e *StandaloneEngine) ExecuteStream(req StandaloneRequest, emit func([]byte) error) error {
	credentials, err := e.orderedCredentials(req.AffinityKey)
	if err != nil {
		return err
	}
	var last error
	for i, c := range credentials {
		streamID := "gateway-" + id()
		ctx := req.Context
		if ctx == nil {
			ctx = context.Background()
		}
		sinkCtx, cancel := context.WithCancel(ctx)
		sink := &standaloneStreamSink{
			ctx:    sinkCtx,
			cancel: cancel,
			events: make(chan standaloneStreamEvent, 16),
		}
		e.sinksMu.Lock()
		e.sinks[streamID] = sink
		e.sinksMu.Unlock()

		r := e.executorRequest(req, c)
		r.StreamID = streamID
		r.Stream = true
		_, startErr := e.service.executeStream(r)
		if startErr != nil {
			cancel()
			e.unregisterSink(streamID)
			last = startErr
			if i < len(credentials)-1 && standaloneRetryable(startErr, c) {
				continue
			}
			return startErr
		}

		emitted := 0
		var streamErr error
		for event := range sink.events {
			if event.done {
				if event.errText != "" {
					streamErr = errors.New(event.errText)
				}
				break
			}
			if len(event.payload) == 0 {
				continue
			}
			if emit != nil {
				if err := emit(event.payload); err != nil {
					streamErr = err
					cancel()
					break
				}
			}
			emitted++
		}
		if streamErr == nil && sinkCtx.Err() != nil {
			streamErr = sinkCtx.Err()
		}
		cancel()
		e.unregisterSink(streamID)
		if streamErr == nil {
			return nil
		}
		last = streamErr
		if emitted == 0 && i < len(credentials)-1 && standaloneRetryableText(streamErr.Error()) {
			continue
		}
		return streamErr
	}
	if last == nil {
		last = fail(503, "no Cline Pass credential completed the request")
	}
	return last
}

func (e *StandaloneEngine) unregisterSink(id string) {
	e.sinksMu.Lock()
	delete(e.sinks, id)
	e.sinksMu.Unlock()
}

func (e *StandaloneEngine) sink(id string) *standaloneStreamSink {
	e.sinksMu.RLock()
	defer e.sinksMu.RUnlock()
	return e.sinks[id]
}

func (e *StandaloneEngine) hostCall(method string, in, out any) error {
	m, _ := in.(map[string]any)
	streamID := str(m["stream_id"])
	sink := e.sink(streamID)
	if sink == nil {
		if method == "host.stream.close" {
			return nil
		}
		return errors.New("standalone stream is unavailable")
	}
	switch method {
	case "host.stream.emit":
		payload, ok := m["payload"].([]byte)
		if !ok {
			return errors.New("standalone stream payload is invalid")
		}
		copyPayload := append([]byte(nil), payload...)
		select {
		case <-sink.ctx.Done():
			return sink.ctx.Err()
		case sink.events <- standaloneStreamEvent{payload: copyPayload}:
			return nil
		}
	case "host.stream.close":
		sink.finish(str(m["error"]))
		return nil
	default:
		return fmt.Errorf("unsupported standalone host callback: %s", method)
	}
}
