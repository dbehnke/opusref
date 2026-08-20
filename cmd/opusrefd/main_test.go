package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/dbehnke/opusref/internal/config"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConfiguredLoggerAppliesLevelAndFormat(t *testing.T) {
	t.Run("json level", func(t *testing.T) {
		var output bytes.Buffer
		logger := newLogger(config.Logging{Level: "warn", Format: "json"}, &output)
		logger.Info("hidden")
		logger.Warn("visible")
		if strings.Contains(output.String(), "hidden") {
			t.Fatal("info message bypassed warn level")
		}
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["level"] != "WARN" || record["msg"] != "visible" {
			t.Fatalf("record=%#v", record)
		}
	})
	t.Run("text debug", func(t *testing.T) {
		var output bytes.Buffer
		logger := newLogger(config.Logging{Level: "debug", Format: "text"}, &output)
		logger.Debug("diagnostic")
		if text := output.String(); !strings.Contains(text, "level=DEBUG") || !strings.Contains(text, "msg=diagnostic") {
			t.Fatalf("output=%q", text)
		}
	})
}

func TestRuntimeLogFieldsDoNotExposeSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	cfg := config.Defaults()
	cfg.Network.UDPListen = "127.0.0.1:46000"
	cfg.Monitoring.HTTPListen = "127.0.0.1:8080"
	cfg.Reflector.ID = "OPUSREF"
	cfg.Authentication.SharedKeyEnv = "SECRET_ENV_NAME"
	cfg.Authentication.SharedKeyFile = "/secret/key/path"
	logRuntimeStart(logger, cfg)
	text := output.String()
	for _, prohibited := range []string{"SECRET_ENV_NAME", "/secret/key/path", "shared_key", "authentication"} {
		if strings.Contains(text, prohibited) {
			t.Fatalf("runtime log exposed %q: %s", prohibited, text)
		}
	}
}

func TestSignalWaitsForRestrictedDrainCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runErrors := make(chan error)
	done := make(chan error, 1)
	go func() { done <- waitForTermination(ctx, cancel, nil, runErrors) }()
	cancel()
	select {
	case <-done:
		t.Fatal("returned before reflector drain completed")
	case <-time.After(25 * time.Millisecond):
	}
	runErrors <- nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not return after reflector drain completed")
	}
}
