package main

import (
	"bytes"
	"context"
	"github.com/dbehnke/opusref/internal/record"
	"github.com/dbehnke/opusref/pkg/server"
	"net"
	"strings"
	"testing"
	"time"
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

func TestTransmitUsesRecordsAndKeepsStdoutClean(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reflector, _ := server.NewReflector(conn, server.ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", ShutdownGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reflector.Run(ctx)
	defer reflector.Close()
	var input, out, diag bytes.Buffer
	if err := record.Write(&input, record.Record{Kind: record.KindAudio, Timestamp: 960, Payload: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	args := []string{"transmit", "--server", conn.LocalAddr().String(), "--node", "N0CALL", "--source", "N0CALL", "--connect-timeout", "2s", "--operation-timeout", "2s"}
	if err := run(args, &input, &out, &diag); err != nil {
		t.Fatalf("%v; stderr=%s", err, diag.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout contains diagnostics: %q", out.String())
	}
}

func TestTransmitRejectsTruncatedInput(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reflector, _ := server.NewReflector(conn, server.ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", ShutdownGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reflector.Run(ctx)
	defer reflector.Close()
	var out, diag bytes.Buffer
	args := []string{"transmit", "--server", conn.LocalAddr().String(), "--node", "N0CALL", "--source", "N0CALL", "--connect-timeout", "2s", "--operation-timeout", "2s"}
	if err := run(args, strings.NewReader("ORR1"), &out, &diag); err == nil {
		t.Fatal("accepted truncated record")
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
