package wire

import (
	"errors"
	"testing"
)

func TestValidatePacketMatrix(t *testing.T) {
	tx := TLV{Type: TLVTransactionID, Value: make([]byte, 8)}
	call := TLV{Type: TLVNodeCallsign, Value: []byte("N0CALL    ")}
	nonce := TLV{Type: TLVClientNonce, Value: make([]byte, 32)}
	tests := []struct {
		name string
		p    Packet
		ctx  ValidationContext
		want Reason
	}{
		{"hello accepted", Packet{Header: Header{Version: 1, Type: PacketHello}, Extensions: []TLV{tx, call, nonce}}, ValidationContext{ClientToServer, PreAdmission}, ""},
		{"hello wrong direction", Packet{Header: Header{Version: 1, Type: PacketHello}, Extensions: []TLV{tx, call, nonce}}, ValidationContext{ServerToClient, PreAdmission}, ReasonUnsupportedType},
		{"audio accepted", Packet{Header: Header{Version: 1, Type: PacketAudio, SessionID: 2, StreamID: 3}, Payload: []byte{1}}, ValidationContext{ClientToServer, Ready}, ""},
		{"audio empty", Packet{Header: Header{Version: 1, Type: PacketAudio, SessionID: 2, StreamID: 3}}, ValidationContext{ClientToServer, Ready}, ReasonMalformed},
		{"audio before ready", Packet{Header: Header{Version: 1, Type: PacketAudio, SessionID: 2, StreamID: 3}, Payload: []byte{1}}, ValidationContext{ClientToServer, Connected}, ReasonInvalidSession},
		{"data needs type", Packet{Header: Header{Version: 1, Type: PacketData, SessionID: 2, StreamID: 3}, Payload: []byte{1}}, ValidationContext{ClientToServer, Ready}, ReasonMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.p, tc.ctx)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Reason != tc.want {
					t.Fatalf("got %v, want reason %s", err, tc.want)
				}
			}
		})
	}
}

func TestValidateRejectsKnownTLVOnWrongPacket(t *testing.T) {
	p := Packet{Header: Header{Version: 1, Type: PacketAudio, SessionID: 1, StreamID: 1}, Extensions: []TLV{{Type: TLVErrorCode, Value: []byte{0, 1}}}, Payload: []byte{1}}
	var ve *ValidationError
	if err := Validate(p, ValidationContext{ClientToServer, Ready}); !errors.As(err, &ve) || ve.Field != "extensions" {
		t.Fatalf("got %v", err)
	}
}

func FuzzValidate(f *testing.F) {
	f.Add([]byte(goldenAudioHex))
	f.Fuzz(func(t *testing.T, text []byte) {
		p, err := Decode(text)
		if err == nil {
			_ = Validate(p, ValidationContext{ClientToServer, Ready})
		}
	})
}
