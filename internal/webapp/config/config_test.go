package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsMatchSpecification(t *testing.T) {
	c := Defaults()
	if c.Web.HTTPListen != "127.0.0.1:8090" || !c.Web.OpenAccess {
		t.Fatalf("unexpected web defaults: %+v", c.Web)
	}
	if c.Storage.Retention.Time() != 24*time.Hour || c.Storage.HardQuotaBytes != 10*1024*1024*1024 {
		t.Fatalf("unexpected storage defaults: %+v", c.Storage)
	}
	if c.Authentication.Argon2MemoryKiB != 65536 || c.Authentication.Argon2Iterations != 3 || c.Authentication.Argon2Parallelism != 4 {
		t.Fatalf("unexpected Argon2 defaults: %+v", c.Authentication)
	}
}

func TestLoadRejectsUnknownAndInsecurePublicBind(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, yaml string }{
		{"unknown", "web:\n  unknown: true\n"},
		{"insecure", "web:\n  http_listen: 0.0.0.0:8090\n  public_origin: http://example.test\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPasskeysRequireValidHTTPSDomain(t *testing.T) {
	c := Defaults()
	c.Web.PublicOrigin = "https://radio.example.org"
	c.WebAuthn.RPID = "example.org"
	c.WebAuthn.Origins = []string{"https://radio.example.org"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.PasskeysEnabled() {
		t.Fatal("passkeys should be enabled")
	}
	c.WebAuthn.RPID = "127.0.0.1"
	if err := c.Validate(); err == nil {
		t.Fatal("IP RP ID must fail")
	}
}

func TestLoadAcceptsDurationStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.yaml")
	data := []byte("storage:\n  retention: 48h\nreflector:\n  monitor_poll_interval: 2s\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Retention.Time() != 48*time.Hour || c.Reflector.MonitorPollInterval.Time() != 2*time.Second {
		t.Fatal("durations were not loaded")
	}
}
