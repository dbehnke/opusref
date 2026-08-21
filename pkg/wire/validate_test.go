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

func TestValidateFlagsPhasesAndNotificationForms(t *testing.T) {
	tx := Uint64TLV(TLVTransactionID, 1)
	call := TLV{Type: TLVNodeCallsign, Value: []byte("N0CALL    ")}
	source := TLV{Type: TLVSourceCallsign, Value: []byte("N0CALL    ")}
	tot := Uint32TLV(TLVTransmitTimeLimit, 180)
	tests := []struct {
		name string
		p    Packet
		ctx  ValidationContext
		ok   bool
	}{
		{"retry request", Packet{Header: Header{Version: 1, Type: PacketKeepalive, Flags: FlagRetry, SessionID: 1}, Extensions: []TLV{tx}}, ValidationContext{ClientToServer, Ready}, true},
		{"retry response", Packet{Header: Header{Version: 1, Type: PacketKeepalive, Flags: FlagResponse | FlagRetry, SessionID: 1}, Extensions: []TLV{tx}}, ValidationContext{ServerToClient, Ready}, false},
		{"response request", Packet{Header: Header{Version: 1, Type: PacketStreamRequest, Flags: FlagResponse, SessionID: 1, StreamID: 1}, Extensions: []TLV{tx, source}}, ValidationContext{ClientToServer, Ready}, false},
		{"full start", Packet{Header: Header{Version: 1, Type: PacketStreamStart, SessionID: 2, StreamID: 1}, Extensions: []TLV{tx, call, source, tot}}, ValidationContext{ServerToClient, Ready}, true},
		{"short start", Packet{Header: Header{Version: 1, Type: PacketStreamStart, SessionID: 2, StreamID: 1}, Extensions: []TLV{tx}}, ValidationContext{ServerToClient, Ready}, false},
		{"start ack", Packet{Header: Header{Version: 1, Type: PacketStreamStart, Flags: FlagResponse, SessionID: 1, StreamID: 1}, Extensions: []TLV{tx}}, ValidationContext{ClientToServer, Ready}, true},
		{"start ack missing response", Packet{Header: Header{Version: 1, Type: PacketStreamStart, SessionID: 1, StreamID: 1}, Extensions: []TLV{tx}}, ValidationContext{ClientToServer, Ready}, false},
		{"media connected", Packet{Header: Header{Version: 1, Type: PacketAudio, SessionID: 1, StreamID: 1}, Payload: []byte{1}}, ValidationContext{ClientToServer, Connected}, false},
		{"audio retry", Packet{Header: Header{Version: 1, Type: PacketAudio, Flags: FlagRetry, SessionID: 1, StreamID: 1}, Payload: []byte{1}}, ValidationContext{ClientToServer, Ready}, false},
		{"data retry", Packet{Header: Header{Version: 1, Type: PacketData, Flags: FlagRetry, SessionID: 1, StreamID: 1}, Extensions: []TLV{Uint16TLV(TLVDataType, 1)}, Payload: []byte{1}}, ValidationContext{ClientToServer, Ready}, false},
		{"end request", Packet{Header: Header{Version: 1, Type: PacketStreamEnd, SessionID: 1, StreamID: 1}, Extensions: []TLV{tx}}, ValidationContext{ClientToServer, Ready}, true},
		{"end response", Packet{Header: Header{Version: 1, Type: PacketStreamEnd, Flags: FlagResponse, SessionID: 1, StreamID: 1}, Extensions: []TLV{tx, Uint16TLV(TLVEndReason, 0)}}, ValidationContext{ServerToClient, Ready}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.p, tc.ctx)
			if (err == nil) != tc.ok {
				t.Fatalf("got %v, ok=%v", err, tc.ok)
			}
		})
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
