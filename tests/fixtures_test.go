package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/dbehnke/opusref/pkg/client"
	"github.com/dbehnke/opusref/pkg/server"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpusFixturesAreOpaqueAndNonempty(t *testing.T) {
	files, err := filepath.Glob("../testdata/opus/*.hex")
	if err != nil || len(files) < 5 {
		t.Fatalf("fixtures: %v, %v", files, err)
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(packet) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		_ = packet
	}
}

func TestFixtureSHA256Manifest(t *testing.T) {
	manifest, err := os.ReadFile("../testdata/opus/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("bad manifest line %q", line)
		}
		data, err := os.ReadFile(filepath.Join("../testdata/opus", fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(data))
		if got != fields[0] {
			t.Fatalf("%s: got %s want %s", fields[1], got, fields[0])
		}
		seen[fields[1]] = true
	}
	files, _ := filepath.Glob("../testdata/opus/*.hex")
	if len(seen) != len(files) {
		t.Fatalf("manifest has %d entries for %d files", len(seen), len(files))
	}
}

func TestEveryOpusFixtureRoutesByteForByte(t *testing.T) {
	r := newRig(t, server.Limits{})
	defer r.close()
	owner := r.client(t, "N0ONE")
	defer owner.Close()
	listener := r.client(t, "N0TWO")
	defer listener.Close()
	if err := owner.RequestStream(context.Background(), "N0ONE"); err != nil {
		t.Fatal(err)
	}
	_ = waitKind(t, listener, client.EventStreamStart)
	files, _ := filepath.Glob("../testdata/opus/*.hex")
	for index, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if err = owner.SendAudio(context.Background(), uint32(index*960), packet); err != nil {
			t.Fatal(err)
		}
		event := waitKind(t, listener, client.EventAudio)
		if !bytes.Equal(packet, event.Payload) {
			t.Fatalf("%s changed", name)
		}
	}
}
