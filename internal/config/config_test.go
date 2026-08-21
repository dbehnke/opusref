package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExample(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.OutboundControlQueuePackets != 64 || cfg.Timers.HealthDeadline == 0 {
		t.Fatalf("bad defaults: %#v", cfg)
	}
}
func TestLoadRejectsUnknownAndInvalidConfiguration(t *testing.T) {
	base, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{string(base) + "\nunknown: true\n", strings.Replace(string(base), "udp_listen: \":46000\"", "udp_listen: \":bad\"", 1), strings.Replace(string(base), "max_clients: 100", "max_clients: 0", 1), strings.Replace(string(base), "keepalive_interval: 10s", "keepalive_interval: 40s", 1), strings.Replace(string(base), "level: \"info\"", "level: \"verbose\"", 1)}
	for _, content := range tests {
		name := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(name); err == nil {
			t.Fatalf("accepted invalid configuration")
		}
	}
}
func TestEnvironmentKeyPrecedesFile(t *testing.T) {
	t.Setenv("TEST_OPUSREF_KEY", "0123456789abcdef")
	cfg := Config{Authentication: Authentication{SharedKeyEnv: "TEST_OPUSREF_KEY", SharedKeyFile: "missing"}}
	got, err := cfg.SharedKey()
	if err != nil || string(got) != "0123456789abcdef" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestKeyFilePermissions(t *testing.T) {
	name := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(name, []byte("0123456789abcdef\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Authentication: Authentication{SharedKeyFile: name}}
	if _, err := cfg.SharedKey(); err == nil {
		t.Fatal("accepted broad permissions")
	}
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := cfg.SharedKey()
	if err != nil || string(got) != "0123456789abcdef" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestKeyFileBoundAndLineEndingRules(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSharedKeyFile(large); err == nil {
		t.Fatal("accepted oversized key")
	}
	cr := filepath.Join(dir, "cr")
	if err := os.WriteFile(cr, []byte("0123456789abcdef\r"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSharedKeyFile(cr)
	if err != nil || string(got) != "0123456789abcdef\r" {
		t.Fatalf("%q %v", got, err)
	}
}
