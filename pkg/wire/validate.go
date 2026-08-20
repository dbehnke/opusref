package wire

import "fmt"

type Direction uint8

const (
	ClientToServer Direction = iota + 1
	ServerToClient
)

type AdmissionPhase uint8

const (
	PreAdmission AdmissionPhase = iota + 1
	Connected
	Ready
)

type ValidationContext struct {
	Direction Direction
	Phase     AdmissionPhase
}
type Reason string

const (
	ReasonMalformed          Reason = "malformed"
	ReasonUnsupportedVersion Reason = "unsupported_version"
	ReasonInvalidSession     Reason = "invalid_session"
	ReasonInvalidStream      Reason = "invalid_stream"
	ReasonUnsupportedType    Reason = "unsupported_type"
	ReasonLimitExceeded      Reason = "limit_exceeded"
)

type ValidationError struct {
	Reason Reason
	Field  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Reason, e.Field) }
func invalid(reason Reason, field string) error {
	return &ValidationError{Reason: reason, Field: field}
}

// Validate checks packet semantics that do not depend on mutable peer state.
func Validate(p Packet, c ValidationContext) error {
	if p.Header.Version != Version1 {
		return invalid(ReasonUnsupportedVersion, "version")
	}
	if c.Direction != ClientToServer && c.Direction != ServerToClient {
		return invalid(ReasonMalformed, "direction")
	}
	if c.Phase < PreAdmission || c.Phase > Ready {
		return invalid(ReasonMalformed, "phase")
	}
	allowed := map[TLVType]bool{}
	require := func(types ...TLVType) {
		for _, typ := range types {
			allowed[typ] = true
		}
	}
	need := []TLVType{}
	fieldsZero := true
	emptyPayload := true
	clientOnly, serverOnly := false, false
	switch p.Header.Type {
	case PacketHello:
		clientOnly = true
		require(TLVTransactionID, TLVNodeCallsign, TLVClientNonce)
		need = []TLVType{TLVTransactionID, TLVNodeCallsign, TLVClientNonce}
	case PacketChallenge:
		serverOnly = true
		require(TLVTransactionID, TLVServerNonce, TLVReflectorID, TLVDisplayName)
		need = []TLVType{TLVTransactionID, TLVServerNonce, TLVReflectorID, TLVDisplayName}
	case PacketAuthenticate:
		clientOnly = true
		require(TLVTransactionID, TLVClientNonce, TLVServerNonce, TLVAuthenticationTag)
		need = []TLVType{TLVTransactionID, TLVClientNonce, TLVServerNonce}
	case PacketWelcome:
		serverOnly = true
		fieldsZero = false
		require(TLVTransactionID, TLVReflectorID, TLVDisplayName)
		need = []TLVType{TLVTransactionID, TLVReflectorID, TLVDisplayName}
	case PacketKeepalive, PacketDisconnect:
		fieldsZero = false
		require(TLVTransactionID)
		need = []TLVType{TLVTransactionID}
	case PacketError:
		fieldsZero = false
		require(TLVErrorCode, TLVTransactionID, TLVErrorText)
		need = []TLVType{TLVErrorCode}
	case PacketStreamRequest:
		clientOnly = true
		fieldsZero = false
		require(TLVTransactionID, TLVSourceCallsign)
		need = []TLVType{TLVTransactionID, TLVSourceCallsign}
	case PacketStreamGrant:
		serverOnly = true
		fieldsZero = false
		require(TLVTransactionID, TLVTransmitTimeLimit)
		need = []TLVType{TLVTransactionID, TLVTransmitTimeLimit}
	case PacketStreamBusy:
		serverOnly = true
		fieldsZero = false
		require(TLVTransactionID)
		need = []TLVType{TLVTransactionID}
	case PacketStreamStart:
		fieldsZero = false
		require(TLVTransactionID, TLVNodeCallsign, TLVSourceCallsign, TLVTransmitTimeLimit)
		need = []TLVType{TLVTransactionID}
	case PacketStreamEnd:
		fieldsZero = false
		require(TLVTransactionID, TLVEndReason)
		need = []TLVType{TLVTransactionID}
	case PacketStreamRevoke:
		fieldsZero = false
		require(TLVTransactionID, TLVEndReason)
		need = []TLVType{TLVTransactionID}
	case PacketAudio:
		fieldsZero = false
		emptyPayload = false
	case PacketData:
		fieldsZero = false
		emptyPayload = false
		require(TLVDataType)
		need = []TLVType{TLVDataType}
	default:
		return invalid(ReasonUnsupportedType, "type")
	}
	if clientOnly && c.Direction != ClientToServer || serverOnly && c.Direction != ServerToClient {
		return invalid(ReasonUnsupportedType, "direction")
	}
	if (p.Header.Type == PacketAudio || p.Header.Type == PacketData) && c.Phase != Ready {
		return invalid(ReasonInvalidSession, "phase")
	}
	if fieldsZero && (p.Header.SessionID != 0 || p.Header.StreamID != 0 || p.Header.Sequence != 0 || p.Header.Timestamp != 0) {
		return invalid(ReasonMalformed, "header")
	}
	if !fieldsZero && p.Header.Type != PacketError && p.Header.SessionID == 0 {
		return invalid(ReasonInvalidSession, "session_id")
	}
	if p.Header.Type >= PacketStreamRequest && p.Header.Type <= PacketData && p.Header.StreamID == 0 {
		return invalid(ReasonInvalidStream, "stream_id")
	}
	if emptyPayload && len(p.Payload) != 0 || !emptyPayload && len(p.Payload) == 0 {
		return invalid(ReasonMalformed, "payload")
	}
	present := map[TLVType]bool{}
	for _, tlv := range p.Extensions {
		base := tlv.Type &^ TLVCriticalMask
		if knownTLV(base) && !allowed[base] {
			return invalid(ReasonMalformed, "extensions")
		}
		present[base] = true
	}
	for _, typ := range need {
		if !present[typ] {
			return invalid(ReasonMalformed, "extensions")
		}
	}
	return nil
}
