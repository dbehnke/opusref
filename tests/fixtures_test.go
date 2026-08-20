package tests

import (
	"bytes"
	"encoding/hex"
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
		forwarded := append([]byte(nil), packet...)
		if !bytes.Equal(packet, forwarded) {
			t.Fatalf("%s changed", name)
		}
	}
}
