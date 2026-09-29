package bridge

import (
	"testing"
	"time"
)

func TestReadTimeoutMeasuresIdleGapNotWholeStream(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	s.mu.Lock()
	s.cfg.TimeoutSeconds = 1
	s.mu.Unlock()

	reads := 0
	s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.stream_read":
			reads++
			if reads <= 3 {
				time.Sleep(400 * time.Millisecond)
				*out.(*readChunk) = readChunk{Payload: []byte("x")}
				return nil
			}
			*out.(*readChunk) = readChunk{Done: true}
			return nil
		case "host.http.stream_close":
			return nil
		default:
			return nil
		}
	})

	start := time.Now()
	if err := s.read(upstreamStream{StreamID: "idle-test"}, func([]byte) error { return nil }); err != nil {
		t.Fatalf("active stream hit total-duration timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1100*time.Millisecond {
		t.Fatalf("test stream ended too quickly to cover the old absolute timeout: %v", elapsed)
	}
}

func TestTimeoutErrorsAreRequestScopedWithoutCredentialCooldown(t *testing.T) {
	rules := requestErrorRules()
	for _, status := range []int{500, 504} {
		found := false
		for _, rule := range rules {
			if rule.Status != status || rule.Action != "stop" {
				continue
			}
			for _, match := range rule.Match {
				if match == "Cline upstream request timed out" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("missing request-scoped timeout rule for status %d: %#v", status, rules)
		}
	}

	auth := authData(Credential{Type: Provider, ID: "credential-1", APIKey: "redacted"}, "credential-1.json").(map[string]any)
	metadata := auth["Metadata"].(map[string]any)
	metaRules, ok := metadata["request_scoped_errors"].([]RequestErrorRule)
	if !ok {
		t.Fatalf("auth metadata request_scoped_errors type = %T, want []RequestErrorRule", metadata["request_scoped_errors"])
	}
	for _, status := range []int{500, 504} {
		found := false
		for _, rule := range metaRules {
			if rule.Status == status && rule.Action == "stop" {
				for _, match := range rule.Match {
					if match == "Cline upstream request timed out" {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("auth metadata omitted timeout stop rule for status %d: %#v", status, metaRules)
		}
	}
}
