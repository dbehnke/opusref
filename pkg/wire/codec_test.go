package wire

import (
	"encoding/hex"
	"testing"
)

const goldenAudioHex = "4f50524601400000002000031122334455667788aabbccdd010203040000bb80f8fffe"
const goldenHelloHex = "4f505246010100000060000000000000000000000000000000000000000000000001000801020304050607080002000a4e3043414c4c20202020000000060020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestEncodeGoldenAudioPacket(t *testing.T) {
	want, err := hex.DecodeString(goldenAudioHex)
	if err != nil {
		t.Fatal(err)
	}
	packet := Packet{
		Header: Header{
			Version:   Version1,
			Type:      PacketAudio,
			SessionID: 0x1122334455667788,
			StreamID:  0xaabbccdd,
			Sequence:  0x01020304,
			Timestamp: 0x0000bb80,
		},
		Payload: []byte{0xf8, 0xff, 0xfe},
	}
	got, err := Encode(packet)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func TestDecodeGoldenAudioPacket(t *testing.T) {
	data, err := hex.DecodeString(goldenAudioHex)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.SessionID != 0x1122334455667788 || packet.Header.StreamID != 0xaabbccdd {
		t.Fatalf("unexpected identifiers: session=%#x stream=%#x", packet.Header.SessionID, packet.Header.StreamID)
	}
	if got := hex.EncodeToString(packet.Payload); got != "f8fffe" {
		t.Fatalf("got payload %s", got)
	}
}

func TestEncodeGoldenHelloPacketWithTLVs(t *testing.T) {
	want, err := hex.DecodeString(goldenHelloHex)
	if err != nil {
		t.Fatal(err)
	}
	packet := Packet{
		Header: Header{Version: Version1, Type: PacketHello},
		Extensions: []TLV{
			{Type: TLVTransactionID, Value: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
			{Type: TLVNodeCallsign, Value: []byte("N0CALL    ")},
			{Type: TLVClientNonce, Value: []byte{
				0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
				16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
			}},
		},
	}
	got, err := Encode(packet)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("got %x, want %x", got, want)
	}
	decoded, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Extensions) != 3 {
		t.Fatalf("got %d extensions, want 3", len(decoded.Extensions))
	}
}

func TestDecodeRejectsMalformedPackets(t *testing.T) {
	valid, err := hex.DecodeString(goldenAudioHex)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"short header":        valid[:BaseHeaderSize-1],
		"bad magic":           append([]byte("NOPE"), valid[4:]...),
		"unsupported version": func() []byte { b := append([]byte(nil), valid...); b[4] = 2; return b }(),
		"reserved flag":       func() []byte { b := append([]byte(nil), valid...); b[7] = 4; return b }(),
		"length mismatch":     valid[:len(valid)-1],
	}
	hello, err := hex.DecodeString(goldenHelloHex)
	if err != nil {
		t.Fatal(err)
	}
	tests["unknown critical TLV"] = func() []byte {
		b := append([]byte(nil), hello...)
		b[32], b[33] = 0x80, 0x7f
		return b
	}()
	tests["nonzero TLV padding"] = func() []byte {
		b := append([]byte(nil), hello...)
		b[58] = 1
		return b
	}()
	tests["duplicate TLV"] = func() []byte {
		b := append([]byte(nil), hello...)
		b[44], b[45] = 0x80, 1
		return b
	}()
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(data); err == nil {
				t.Fatal("Decode accepted malformed packet")
			}
		})
	}
}

func TestUnknownOptionalZeroLengthTLVRoundTrips(t *testing.T) {
	packet := Packet{
		Header:     Header{Version: Version1, Type: PacketAudio, SessionID: 1, StreamID: 1},
		Extensions: []TLV{{Type: 0x1234}},
		Payload:    []byte{0xf8},
	}
	data, err := Encode(packet)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Extensions) != 1 || len(decoded.Extensions[0].Value) != 0 {
		t.Fatalf("unexpected extensions: %#v", decoded.Extensions)
	}
}

func TestEncodeRejectsInvalidKnownTLVValues(t *testing.T) {
	tests := map[string]TLV{
		"short transaction": {Type: TLVTransactionID, Value: make([]byte, 7)},
		"invalid callsign":  {Type: TLVNodeCallsign, Value: []byte("N0CALL !  ")},
		"short nonce":       {Type: TLVClientNonce, Value: make([]byte, 31)},
		"invalid UTF-8":     {Type: TLVDisplayName, Value: []byte{0xff}},
		"zero data type":    {Type: TLVDataType, Value: []byte{0, 0}},
		"bad end reason":    {Type: TLVEndReason, Value: []byte{0, 6}},
	}
	for name, extension := range tests {
		t.Run(name, func(t *testing.T) {
			packet := Packet{
				Header:     Header{Version: Version1, Type: PacketAudio, SessionID: 1, StreamID: 1},
				Extensions: []TLV{extension},
				Payload:    []byte{0xf8},
			}
			if _, err := Encode(packet); err == nil {
				t.Fatal("Encode accepted an invalid registered TLV")
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	seed, err := hex.DecodeString(goldenAudioHex)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("OPRF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		packet, err := Decode(data)
		if err != nil {
			return
		}
		encoded, err := Encode(packet)
		if err != nil {
			t.Fatalf("decoded packet cannot be encoded: %v", err)
		}
		if len(encoded) > MaxDatagramSize {
			t.Fatalf("encoded datagram is too large: %d", len(encoded))
		}
	})
}
