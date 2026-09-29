package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStandaloneFixture(t *testing.T) StandaloneConfig {
	t.Helper()
	root := t.TempDir()
	control := filepath.Join(root, "control")
	authDir := filepath.Join(root, "auths")
	dataDir := filepath.Join(root, "gateway-data")
	for _, dir := range []string{control, authDir, dataDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.DataDir = "/ignored-by-standalone"
	cfg.TransportMode = "direct"
	cfg.LogRetention = 100
	cfg.Models = []Model{{
		ID:         "deepseek-v4.1-flash",
		UpstreamID: "cline-pass/deepseek-v4.1-flash",
	}}
	settingsPath := filepath.Join(control, "settings.json")
	if err := os.WriteFile(settingsPath, jsonBytes(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"cline-a", "cline-b"} {
		c := Credential{
			Type:   Provider,
			ID:     id,
			Label:  id,
			APIKey: "test-key-" + id,
		}
		if i == 1 {
			c.ProxyURL = ""
		}
		if err := os.WriteFile(filepath.Join(authDir, id+".json"), jsonBytes(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return StandaloneConfig{SettingsPath: settingsPath, AuthDir: authDir, DataDir: dataDir}
}

func TestStandaloneAffinityIsStableAndRoundRobinHasFallback(t *testing.T) {
	engine, err := NewStandaloneEngine(writeStandaloneFixture(t))
	if err != nil {
		t.Fatalf("NewStandaloneEngine: %v", err)
	}
	first, err := engine.orderedCredentials("session:alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.orderedCredentials("session:alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 || first[0].ID != second[0].ID {
		t.Fatalf("affinity selection is not stable: first=%v second=%v", first, second)
	}

	rr1, err := engine.orderedCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	rr2, err := engine.orderedCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rr1) != 2 || len(rr2) != 2 || rr1[0].ID == rr2[0].ID {
		t.Fatalf("round-robin fallback did not rotate: first=%v second=%v", rr1, rr2)
	}
}

func TestStandaloneReloadsControlPlaneCredentialsReadOnly(t *testing.T) {
	cfg := writeStandaloneFixture(t)
	engine, err := NewStandaloneEngine(cfg)
	if err != nil {
		t.Fatalf("NewStandaloneEngine: %v", err)
	}
	status := engine.Status()
	if status.EnabledCredentials != 2 || status.ModelCount != 1 {
		t.Fatalf("unexpected initial status: %#v", status)
	}

	disabled := Credential{
		Type:     Provider,
		ID:       "cline-a",
		Label:    "cline-a",
		APIKey:   "test-key-cline-a",
		Disabled: true,
	}
	if err := os.WriteFile(filepath.Join(cfg.AuthDir, "cline-a.json"), jsonBytes(disabled), 0600); err != nil {
		t.Fatal(err)
	}
	status = engine.Status()
	if status.EnabledCredentials != 1 {
		t.Fatalf("credential reload did not observe control-plane change: %#v", status)
	}
}
