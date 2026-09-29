package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type directHTTPStream struct {
	body  io.ReadCloser
	close func()
}

var directClientCache sync.Map

func directClient(proxyURL string) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	if cached, ok := directClientCache.Load(key); ok {
		return cached.(*http.Client), nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fail(500, "default HTTP transport is unavailable")
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	if key != "" {
		proxy, err := url.Parse(key)
		if err != nil || proxy.Scheme == "" || proxy.Host == "" {
			return nil, fail(400, "invalid credential proxy URL")
		}
		switch strings.ToLower(proxy.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fail(400, "unsupported credential proxy scheme")
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{Transport: transport}
	actual, _ := directClientCache.LoadOrStore(key, client)
	return actual.(*http.Client), nil
}

func (s *Service) openDirectUpstream(parent context.Context, c Credential, body []byte, deadline time.Time, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	client, err := directClient(c.ProxyURL)
	if err != nil {
		return upstreamStream{}, err
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancelContext := context.WithDeadline(parent, deadline)
	done := make(chan struct{})
	var closeOnce sync.Once
	closeStream := func() {
		closeOnce.Do(func() {
			close(done)
			cancelContext()
		})
	}
	go func() {
		select {
		case <-s.stopCh:
			closeStream()
		case <-done:
		}
	}()

	cfg := s.config()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		closeStream()
		return upstreamStream{}, fail(500, "build direct upstream request: "+safeError(err))
	}
	req.Header = headers(c)
	resp, err := client.Do(req)
	if err != nil {
		closeStream()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return upstreamStream{}, fail(504, "Cline upstream request timed out")
		}
		select {
		case <-s.stopCh:
			return upstreamStream{}, fail(503, "ClinePassBridge is shutting down")
		default:
		}
		return upstreamStream{}, fail(502, "direct upstream transport failed: "+safeError(err))
	}
	out := upstreamStream{
		deadline: deadline, requestBytes: int64(len(body)),
		StatusCode: resp.StatusCode, Headers: resp.Header.Clone(),
		direct: &directHTTPStream{body: resp.Body, close: closeStream},
	}
	if len(diagnostics) > 0 {
		out.diagnostics = diagnostics[0]
	}
	return out, nil
}

func (s *Service) readDirect(up upstreamStream, fn func([]byte) error) error {
	if up.direct == nil || up.direct.body == nil {
		return fail(502, "direct upstream stream is unavailable")
	}
	defer up.direct.body.Close()
	defer up.direct.close()
	total := 0
	buf := make([]byte, 64*1024)
	for {
		n, err := up.direct.body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			total += n
			if up.diagnostics != nil {
				up.diagnostics.capture(chunk)
			}
			if total > s.config().MaxResponseBytes {
				return fail(502, "upstream response exceeds configured limit")
			}
			if fn != nil {
				if callErr := fn(chunk); callErr != nil {
					return callErr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if !up.deadline.IsZero() && time.Now().After(up.deadline) {
				return fail(504, "Cline upstream request timed out")
			}
			select {
			case <-s.stopCh:
				return fail(503, "ClinePassBridge is shutting down")
			default:
			}
			return fail(502, "direct upstream stream interrupted: "+safeError(err))
		}
	}
}
