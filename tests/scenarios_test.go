package tests

import "testing"

// These skipped cases reserve the normative interoperability test matrix.
// Each skip must be replaced by an executable test with implementation work.
func TestProtocolScenarios(t *testing.T) {
	cases := []string{
		"golden header and TLV vectors",
		"malformed length and TLV rejection",
		"unsupported version rejection",
		"handshake retry idempotence",
		"simultaneous floor contention",
		"grant inactivity and transmit timeout release",
		"late join stream metadata",
		"data rejected outside active stream",
		"sequence wrap and loss accounting",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Skip("protocol implementation is outside the bootstrap scope")
		})
	}
}

func FuzzWireDecoder(f *testing.F) {
	f.Add([]byte("OPRF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Skip("wire decoder is outside the bootstrap scope")
	})
}
