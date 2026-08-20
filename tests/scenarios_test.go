package tests

import "testing"

// These skipped cases reserve the normative interoperability test matrix.
// Each skip must be replaced by an executable test with implementation work.
func TestProtocolScenarios(t *testing.T) {
	cases := []string{
		"handshake retry idempotence",
		"simultaneous floor contention",
		"grant inactivity and transmit time limit release",
		"late join stream metadata",
		"stream metadata acknowledgement and retry",
		"data rejected outside active stream",
		"sequence wrap and loss accounting",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Skip("protocol implementation is outside the bootstrap scope")
		})
	}
}
