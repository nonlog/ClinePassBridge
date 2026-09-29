package bridge

import "testing"

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
