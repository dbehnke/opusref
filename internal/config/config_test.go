package config

import (
	"os"
	"path/filepath"
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
