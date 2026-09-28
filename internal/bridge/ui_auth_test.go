package bridge

import (
    "strings"
    "testing"
)

func TestCPAAuthHelperSupportsCurrentSecureStorage(t *testing.T) {
    raw, err := ui.ReadFile("ui/cpa-auth.js")
    if err != nil {
        t.Fatalf("read auth helper: %v", err)
    }
    page := string(raw)
    for _, needle := range []string{
        "enc::v2::",
        "cli-proxy-api-webui::secure-storage|v2|",
        "enc::v1::",
        "cli-proxy-auth",
    } {
        if !strings.Contains(page, needle) {
            t.Fatalf("auth helper is missing %q", needle)
        }
    }
}
