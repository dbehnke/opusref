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

func TestOrderedShutdownStopsWorkBeforeClosingHTTP(t *testing.T) {
	var order []string
	step := func(name string) func() { return func() { order = append(order, name) } }
	errStep := func(name string) func() error { return func() error { order = append(order, name); return nil } }
	if err := orderedShutdown(shutdownSteps{beginDrain: step("drain"), stopPTT: errStep("ptt"), closeWSS: step("wss"), archive: step("archive"), receiver: errStep("receive"), transmitter: errStep("transmit"), cancel: step("cancel"), http: errStep("http")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain", "ptt", "wss", "archive", "receive", "transmit", "cancel", "http"}
	if len(order) != len(want) {
		t.Fatalf("order=%v", order)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("order=%v want=%v", order, want)
		}
	}
}
