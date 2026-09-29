package bridge

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

type upstreamStream struct {
	deadline    time.Time
	diagnostics *modelTestDiagnostics
	requestBytes int64
	StatusCode  int         `json:"status_code"`
	Headers     http.Header `json:"headers"`
	StreamID    string      `json:"stream_id"`
}
type readChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func (s *Service) prepare(r ExecutorRequest) (map[string]any, Credential, string, error) {
	c, e := s.selectedCredential(r)
	if e != nil {
		return nil, c, "", e
	}
	up, e := s.resolveModel(r.Model)
	if e != nil {
		return nil, c, "", e
	}
	j, e := decodeObject(r.Payload)
	if e != nil {
		return nil, c, "", fail(400, "invalid request JSON")
	}
	if len(list(j["messages"])) == 0 {
		return nil, c, "", fail(400, "messages must be a nonempty array")
	}
	j["model"] = up
	// Current Pass planner silently ignores these. Never preserve a false promise of strict routing.
	if p := object(j["provider"]); len(p) > 0 {
		return nil, c, "", fail(400, "provider pinning is unavailable for this Cline Pass integration")
	}
	if p := object(object(j["providerOptions"])["gateway"]); len(p) > 0 {
		return nil, c, "", fail(400, "providerOptions.gateway pinning is currently ignored by Cline; use automatic routing")
	}
	return j, c, up, nil
}
func headers(c Credential) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + c.APIKey}, "Content-Type": []string{"application/json"}, "User-Agent": []string{"ClinePassBridge/" + Version}}
}
func (s *Service) request(r ExecutorRequest, c Credential, j map[string]any, stream bool, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	j["stream"] = stream
	if stream {
		opts := object(j["stream_options"])
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		j["stream_options"] = opts
	} else {
		delete(j, "stream_options")
	}
	body := jsonBytes(j)
	up, err := s.openUpstream(map[string]any{"host_callback_id": r.HostCallbackID, "method": "POST", "url": s.config().BaseURL + "/chat/completions", "headers": headers(c), "body": body}, r.deadline, diagnostics...)
	up.requestBytes = int64(len(body))
	return up, err
}

func (s *Service) openUpstream(payload any, deadline time.Time, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	type result struct {
		up  upstreamStream
		err error
	}
	results := make(chan result)
	abandoned := make(chan struct{})
	defer close(abandoned)
	if err := s.begin(); err != nil {
		return upstreamStream{}, err
	}
	go func() {
		defer s.active.Done()
		var up upstreamStream
		err := s.call("host.http.do_stream", payload, &up)
		select {
		case results <- result{up, err}:
		case <-abandoned:
			s.closeUpstream(up.StreamID)
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var out upstreamStream
	select {
	case result := <-results:
		out = result.up
		if len(diagnostics) > 0 {
			out.diagnostics = diagnostics[0]
		}
		if result.err != nil {
			if out.diagnostics != nil {
				out.diagnostics.transportError(result.err.Error())
			}
			s.closeUpstream(out.StreamID)
			return out, fail(502, "upstream transport failed: "+safeError(result.err))
		}
	case <-timer.C:
		return out, fail(504, "Cline upstream request timed out")
	case <-s.stopCh:
		return out, fail(503, "ClinePassBridge is shutting down")
	}
	out.deadline = deadline
	if out.StreamID == "" {
		return out, fail(502, "host returned no upstream stream")
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, fail(503, "ClinePassBridge is shutting down")
	}
	return out, nil
}
func (s *Service) closeUpstream(id string) {
	if id != "" {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}
}
func (s *Service) read(up upstreamStream, fn func([]byte) error) error {
	cfg := s.config()
	var timedOut bool
	var mu sync.Mutex
	deadline := up.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	}
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() {
		defer close(timerDone)
		mu.Lock()
		timedOut = true
		mu.Unlock()
		s.closeUpstream(up.StreamID)
	})
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
	}()
	defer s.closeUpstream(up.StreamID)
	total := 0
	for {
		var chunk readChunk
		e := s.call("host.http.stream_read", map[string]any{"stream_id": up.StreamID}, &chunk)
		mu.Lock()
		timeout := timedOut
		mu.Unlock()
		if timeout {
			return fail(504, "Cline upstream request timed out")
		}
		if e != nil {
			if up.diagnostics != nil {
				up.diagnostics.transportError(e.Error())
			}
			return fail(502, "upstream read failed: "+safeError(e))
		}
		if chunk.Error != "" {
			if up.diagnostics != nil {
				up.diagnostics.transportError(chunk.Error)
			}
			return fail(502, "upstream stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		total += len(chunk.Payload)
		if up.diagnostics != nil {
			up.diagnostics.capture(chunk.Payload)
		}
		if total > cfg.MaxResponseBytes {
			return fail(502, "upstream response exceeds configured limit")
		}
		if len(chunk.Payload) > 0 {
			if e = fn(chunk.Payload); e != nil {
				return e
			}
		}
		if chunk.Done {
			return nil
		}
	}
}
func (s *Service) readJSON(up upstreamStream) ([]byte, error) {
	var b bytes.Buffer
	e := s.read(up, func(v []byte) error { b.Write(v); return nil })
	if e != nil {
		return nil, e
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		j, _ := decodeObject(b.Bytes())
		return nil, fail(up.StatusCode, errorMessage(j))
	}
	return b.Bytes(), nil
}
func (s *Service) newLog(r ExecutorRequest, c Credential, up string) LogEntry {
	return LogEntry{
		ID: id(), Time: time.Now().UTC(), Model: r.Model, UpstreamModel: up, Stream: r.Stream,
		Provider: "unknown", ProviderSource: "not_reported", Credential: c.Label, Attempts: []Attempt{},
		RequestPath: str(r.Metadata["request_path"]), SourceFormat: r.SourceFormat, OutputFormat: r.Format,
		OriginalBytes: int64(len(r.OriginalRequest)), PayloadBytes: int64(len(r.Payload)),
	}
}
func (s *Service) execute(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	r.Stream = false
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	start := time.Now()
	prepareStarted := time.Now()
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	entry.PrepareMS = time.Since(prepareStarted).Milliseconds()
	defer func() {
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Status = statusOf(e)
		entry.Error = safeError(e)
		s.appendLog(entry)
	}()
	if e != nil {
		return nil, e
	}
	cfg := s.config()
	wantStream := cfg.NonstreamMode == "stream-aggregate"
	var body []byte
	for n := 0; n < 2; n++ {
		attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported", Mode: "nonstream"}
		if wantStream {
			attempt.Mode = "stream-aggregate"
		}
		t := time.Now()
		var us upstreamStream
		openStarted := time.Now()
		us, e = s.request(r, c, j, wantStream)
		entry.UpstreamOpenMS += time.Since(openStarted).Milliseconds()
		if us.requestBytes > entry.UpstreamBytes {
			entry.UpstreamBytes = us.requestBytes
		}
		if e == nil {
			if wantStream {
				var cp *completion
				cp, e = s.consumeSSE(us, r.Model, &entry, &attempt, start, nil)
				if e == nil {
					body, e = cp.result(r.Model)
				}
			} else {
				var raw []byte
				raw, e = s.readJSON(us)
				if e == nil {
					var o map[string]any
					o, e = unwrap(raw)
					if e == nil {
						observeMetadata(o, &entry, &attempt)
						o["model"] = r.Model
						body = jsonBytes(o)
					}
				}
			}
		}
		attempt.Status = statusOf(e)
		attempt.Error = safeError(e)
		attempt.DurationMS = time.Since(t).Milliseconds()
		entry.Attempts = append(entry.Attempts, attempt)
		if e == nil {
			break
		}
		if n == 0 && !wantStream && cfg.NonstreamMode == "native-fallback" && isEmptyError(e) {
			wantStream = true
			continue
		}
		break
	}
	if e != nil {
		return nil, e
	}
	if claudeOutputRequested(r.Format) {
		body, e = openAICompletionToClaude(body, r.Model, r.OriginalRequest)
		if e != nil {
			return nil, e
		}
	} else if responsesOutputRequested(r.Format) {
		body, e = openAICompletionToResponses(body, r.Model, r.OriginalRequest, r.Payload)
		if e != nil {
			return nil, e
		}
	}
	return Response{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}
func (s *Service) consumeSSE(us upstreamStream, model string, entry *LogEntry, attempt *Attempt, start time.Time, emit func([]byte) error) (*completion, error) {
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e := s.readJSON(us)
		return nil, e
	}
	if !strings.Contains(strings.ToLower(us.Headers.Get("Content-Type")), "text/event-stream") {
		_, e := s.readJSON(us)
		if e != nil {
			return nil, e
		}
		return nil, fail(502, "Cline returned non-SSE content for a streaming request")
	}
	cp := newCompletion()
	decoder := SSEDecoder{max: s.config().MaxResponseBytes}
	e := s.read(us, func(b []byte) error {
		if entry.FirstUpstreamMS == 0 && len(b) > 0 {
			entry.FirstUpstreamMS = time.Since(start).Milliseconds()
		}
		return decoder.Feed(b, func(payload []byte, event string) error {
			if strings.TrimSpace(string(payload)) == "[DONE]" {
				cp.done = true
				if !cp.allFinished() {
					return fail(502, "upstream stream ended without finish_reason")
				}
				// CPA owns the downstream SSE envelope and terminal marker.
				return errStreamDone
			}
			j, err := decodeObject(payload)
			if err != nil {
				return err
			}
			if entry.FirstChoiceMS == 0 && len(list(j["choices"])) > 0 {
				entry.FirstChoiceMS = time.Since(start).Milliseconds()
			}
			if j["error"] != nil || event == "error" || j["success"] == false {
				return fail(502, errorMessage(j))
			}
			observeMetadata(j, entry, attempt)
			cp.observe(j)
			if entry.TTFTMS == 0 && contentStarted(j) {
				entry.TTFTMS = time.Since(start).Milliseconds()
			}
			if emit != nil {
				j["model"] = model
				return emit(jsonBytes(j))
			}
			return nil
		})
	})
	if errors.Is(e, errStreamDone) {
		e = nil
	}
	if e == nil && !cp.done {
		e = decoder.End()
		if e == nil {
			e = fail(502, "upstream stream closed before [DONE]")
		}
	}
	return cp, e
}
func (s *Service) executeStream(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	r.Stream = true
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	start := time.Now()
	prepareStarted := time.Now()
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	entry.PrepareMS = time.Since(prepareStarted).Milliseconds()
	failEarly := func(err error) (any, error) {
		s.active.Done()
		entry.Status = statusOf(err)
		entry.Error = safeError(err)
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Attempts = append(entry.Attempts, Attempt{Status: entry.Status, Error: entry.Error, Mode: "stream", Provider: "unknown", DurationMS: entry.DurationMS})
		s.appendLog(entry)
		return nil, err
	}
	if e != nil {
		return failEarly(e)
	}
	if r.StreamID == "" {
		return failEarly(fail(500, "host provided no output stream identifier"))
	}
	openStarted := time.Now()
	us, e := s.request(r, c, j, true)
	entry.UpstreamOpenMS = time.Since(openStarted).Milliseconds()
	entry.UpstreamBytes = us.requestBytes
	if e != nil {
		return failEarly(e)
	}
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e = s.readJSON(us)
		return failEarly(e)
	}
	nativeClaude := claudeOutputRequested(r.Format)
	nativeResponses := responsesOutputRequested(r.Format)
	var claudeConverter *claudeStreamConverter
	var responsesConverter *responsesStreamConverter
	if nativeClaude {
		claudeConverter = newClaudeStreamConverter(r.Model, r.OriginalRequest)
	}
	if nativeResponses {
		responsesConverter = newResponsesStreamConverter(r.Model, r.OriginalRequest, r.Payload)
	}
	go func() {
		defer s.active.Done()
		attempt := Attempt{Mode: "stream", Provider: "unknown", ProviderSource: "not_reported"}
		var err error
		defer func() {
			if recover() != nil {
				err = fail(500, "ClinePassBridge stream processing failed")
			}
			attempt.Status = statusOf(err)
			attempt.Error = safeError(err)
			attempt.DurationMS = time.Since(start).Milliseconds()
			entry.Attempts = append(entry.Attempts, attempt)
			entry.Status = attempt.Status
			entry.Error = attempt.Error
			entry.DurationMS = attempt.DurationMS
			s.appendLog(entry)
			_ = s.call("host.stream.close", map[string]any{"stream_id": r.StreamID, "error": safeError(err)}, nil)
		}()
		emitDownstream := func(payload []byte) error {
			if entry.FirstEmitMS == 0 {
				entry.FirstEmitMS = time.Since(start).Milliseconds()
			}
			emitStarted := time.Now()
			e := s.call("host.stream.emit", map[string]any{"stream_id": r.StreamID, "payload": payload}, nil)
			emitWait := time.Since(emitStarted).Milliseconds()
			if entry.FirstEmitDoneMS == 0 {
				entry.FirstEmitDoneMS = time.Since(start).Milliseconds()
			}
			entry.EmitCalls++
			entry.EmitWaitMS += emitWait
			if emitWait > entry.EmitWaitMaxMS {
				entry.EmitWaitMaxMS = emitWait
			}
			if e != nil {
				return fail(499, "client disconnected")
			}
			return nil
		}
		_, err = s.consumeSSE(us, r.Model, &entry, &attempt, start, func(b []byte) error {
			if nativeClaude {
				events, convertErr := claudeConverter.Feed(b)
				if convertErr != nil {
					return convertErr
				}
				for _, event := range events {
					if emitErr := emitDownstream(event); emitErr != nil {
						return emitErr
					}
				}
				return nil
			}
			if nativeResponses {
				events, convertErr := responsesConverter.Feed(b)
				if convertErr != nil {
					return convertErr
				}
				for _, event := range events {
					if emitErr := emitDownstream(event); emitErr != nil {
						return emitErr
					}
				}
				return nil
			}
			// Older hosts may still request Chat Completions output for /v1/messages.
			// Preserve the legacy bridge in that compatibility path only.
			if str(r.Metadata["request_path"]) == "/v1/messages" {
				b = append(append([]byte("data: "), b...), []byte("\n\n")...)
			}
			return emitDownstream(b)
		})
		if err == nil && nativeClaude {
			var events [][]byte
			events, err = claudeConverter.Done()
			if err == nil {
				for _, event := range events {
					if emitErr := emitDownstream(event); emitErr != nil {
						err = emitErr
						break
					}
				}
			}
		}
		if err == nil && nativeResponses {
			var events [][]byte
			events, err = responsesConverter.Done()
			if err == nil {
				for _, event := range events {
					if emitErr := emitDownstream(event); emitErr != nil {
						err = emitErr
						break
					}
				}
			}
		}

	}()
	return map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}}}, nil
}
