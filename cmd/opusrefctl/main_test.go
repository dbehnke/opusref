package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandRejectsMissingRequiredFlagsWithoutWritingStdout(t *testing.T) {
	var out, diag bytes.Buffer
	err := run([]string{"listen"}, strings.NewReader(""), &out, &diag)
	if err == nil {
		t.Fatal("accepted missing flags")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout: %q", out.String())
	}
}
func TestCommandRejectsUnknownSubcommand(t *testing.T) {
	if err := run([]string{"unknown"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted unknown command")
	}
}
