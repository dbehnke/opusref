package main

import "testing"

func TestRunRequiresKnownCommand(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("missing command accepted")
	}
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}
