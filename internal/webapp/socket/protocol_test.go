package socket

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBinaryRoundTripPreservesPacket(t *testing.T) {
	want := Media{Kind: KindTransmit, ChannelID: 9, Sequence: 7, Timestamp: 960, Payload: []byte{0xf8, 0xff}}
	encoded, err := EncodeMedia(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeMedia(encoded, ClientToServer)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || got.ChannelID != want.ChannelID || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %#v", got)
	}
}
func TestBinaryValidation(t *testing.T) {
	valid, _ := EncodeMedia(Media{Kind: KindTransmit, ChannelID: 1, Payload: []byte{1}})
	cases := [][]byte{valid[:31], append(valid, 0), append([]byte("NOPE"), valid[4:]...)}
	reserved := append([]byte(nil), valid...)
	reserved[26] = 1
	cases = append(cases, reserved)
	zero := append([]byte(nil), valid...)
	binary.BigEndian.PutUint64(zero[8:16], 0)
	cases = append(cases, zero)
	for i, data := range cases {
		if _, err := DecodeMedia(data, ClientToServer); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func FuzzDecodeMedia(f *testing.F) {
	seed, _ := EncodeMedia(Media{Kind: KindTransmit, ChannelID: 1, Payload: []byte{1}})
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = DecodeMedia(data, ClientToServer) })
}

func TestControlRejectsDuplicateKeysAndTrailingJSON(t *testing.T) {
	for _, data := range []string{`{"api_version":1,"type":"hello","request_id":"a","body":{},"type":"hello"}`, `{"api_version":1,"type":"hello","request_id":"a","body":{}} {}`} {
		if _, err := DecodeControl([]byte(data), ClientToServer); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
