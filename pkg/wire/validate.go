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
	ReasonMalformed           Reason = "malformed"
	ReasonUnsupportedVersion  Reason = "unsupported_version"
	ReasonInvalidSession      Reason = "invalid_session"
	ReasonInvalidStream       Reason = "invalid_stream"
	ReasonUnsupportedType     Reason = "unsupported_type"
	ReasonLimitExceeded       Reason = "limit_exceeded"
	ReasonTransactionConflict Reason = "transaction_conflict"
)

type ValidationError struct {
	Reason Reason
	Field  string
}

func (e *ValidationError) Error() string        { return fmt.Sprintf("%s: %s", e.Reason, e.Field) }
func invalid(reason Reason, field string) error { return &ValidationError{reason, field} }

type packetRule struct {
	directions                                  uint8
	phases                                      uint8
	request, response                           bool
	session, stream                             string
	sequence                                    string
	payload                                     string
	requestRequired, responseRequired, optional []TLVType
}

const (
	dirC           = 1
	dirS           = 2
	phasePre       = 1
	phaseConnected = 2
	phaseReady     = 4
)

var rules = map[PacketType]packetRule{
	PacketHello:         {dirC, phasePre, true, false, "zero", "zero", "zero", "empty", []TLVType{TLVTransactionID, TLVNodeCallsign, TLVClientNonce}, nil, nil},
	PacketChallenge:     {dirS, phasePre, false, true, "zero", "zero", "zero", "empty", nil, []TLVType{TLVTransactionID, TLVServerNonce, TLVReflectorID, TLVDisplayName}, nil},
	PacketAuthenticate:  {dirC, phasePre, true, false, "zero", "zero", "zero", "empty", []TLVType{TLVTransactionID, TLVClientNonce, TLVServerNonce}, nil, []TLVType{TLVAuthenticationTag}},
	PacketWelcome:       {dirS, phasePre | phaseConnected, false, true, "nonzero", "zero", "zero", "empty", nil, []TLVType{TLVTransactionID, TLVReflectorID, TLVDisplayName}, nil},
	PacketKeepalive:     {dirC | dirS, phaseConnected | phaseReady, true, true, "nonzero", "zero", "zero", "empty", []TLVType{TLVTransactionID}, []TLVType{TLVTransactionID}, nil},
	PacketDisconnect:    {dirC | dirS, phaseConnected | phaseReady, true, true, "nonzero", "zero", "zero", "empty", []TLVType{TLVTransactionID}, []TLVType{TLVTransactionID}, nil},
	PacketError:         {dirC | dirS, phasePre | phaseConnected | phaseReady, false, true, "any", "any", "zero", "empty", nil, []TLVType{TLVErrorCode}, []TLVType{TLVTransactionID, TLVErrorText}},
	PacketStreamRequest: {dirC, phaseReady, true, false, "nonzero", "nonzero", "zero", "empty", []TLVType{TLVTransactionID, TLVSourceCallsign}, nil, nil},
	PacketStreamGrant:   {dirS, phaseReady, false, true, "nonzero", "nonzero", "zero", "empty", nil, []TLVType{TLVTransactionID, TLVTransmitTimeLimit}, nil},
	PacketStreamBusy:    {dirS, phaseReady, false, true, "nonzero", "nonzero", "zero", "empty", nil, []TLVType{TLVTransactionID}, nil},
	PacketStreamStart:   {dirC | dirS, phaseReady, true, true, "nonzero", "nonzero", "zero", "empty", []TLVType{TLVTransactionID, TLVNodeCallsign, TLVSourceCallsign, TLVTransmitTimeLimit}, []TLVType{TLVTransactionID}, nil},
	PacketStreamEnd:     {dirC | dirS, phaseReady, true, true, "nonzero", "nonzero", "any", "empty", []TLVType{TLVTransactionID}, []TLVType{TLVTransactionID, TLVEndReason}, nil},
	PacketStreamRevoke:  {dirC | dirS, phaseReady, true, true, "nonzero", "nonzero", "any", "empty", []TLVType{TLVTransactionID, TLVEndReason}, []TLVType{TLVTransactionID}, nil},
	PacketAudio:         {dirC | dirS, phaseReady, true, false, "nonzero", "nonzero", "any", "nonempty", nil, nil, nil},
	PacketData:          {dirC | dirS, phaseReady, true, false, "nonzero", "nonzero", "any", "nonempty", []TLVType{TLVDataType}, nil, nil},
}

// Validate checks all stateless rules in the v1 packet matrix.
func Validate(p Packet, c ValidationContext) error {
	if p.Header.Version != Version1 {
		return invalid(ReasonUnsupportedVersion, "version")
	}
	rule, ok := rules[p.Header.Type]
	if !ok {
		return invalid(ReasonUnsupportedType, "type")
	}
	direction := uint8(0)
	if c.Direction == ClientToServer {
		direction = dirC
	} else if c.Direction == ServerToClient {
		direction = dirS
	} else {
		return invalid(ReasonMalformed, "direction")
	}
	phase := uint8(0)
	if c.Phase == PreAdmission {
		phase = phasePre
	} else if c.Phase == Connected {
		phase = phaseConnected
	} else if c.Phase == Ready {
		phase = phaseReady
	} else {
		return invalid(ReasonMalformed, "phase")
	}
	if rule.directions&direction == 0 {
		return invalid(ReasonUnsupportedType, "direction")
	}
	if rule.phases&phase == 0 {
		return invalid(ReasonInvalidSession, "phase")
	}
	response := p.Header.Flags&FlagResponse != 0
	if p.Header.Flags&FlagReservedMask != 0 || response && p.Header.Flags&FlagRetry != 0 || response && !rule.response || !response && !rule.request {
		return invalid(ReasonMalformed, "flags")
	}
	if (p.Header.Type == PacketStreamStart || p.Header.Type == PacketStreamRevoke) && ((direction == dirC) != response) {
		return invalid(ReasonMalformed, "flags")
	}
	if p.Header.Type == PacketStreamEnd && ((direction == dirS) != response) {
		return invalid(ReasonMalformed, "flags")
	}
	if checkNumber(rule.session, p.Header.SessionID) != nil {
		return invalid(ReasonInvalidSession, "session_id")
	}
	if checkNumber(rule.stream, uint64(p.Header.StreamID)) != nil {
		return invalid(ReasonInvalidStream, "stream_id")
	}
	if rule.sequence == "zero" && (p.Header.Sequence != 0 || p.Header.Timestamp != 0) {
		return invalid(ReasonMalformed, "sequence")
	}
	if rule.payload == "empty" && len(p.Payload) != 0 || rule.payload == "nonempty" && len(p.Payload) == 0 {
		return invalid(ReasonMalformed, "payload")
	}
	required := rule.requestRequired
	if response {
		required = rule.responseRequired
	}
	allowed := map[TLVType]bool{}
	for _, typ := range append(append([]TLVType{}, required...), rule.optional...) {
		allowed[typ] = true
	}
	present := map[TLVType]bool{}
	for _, tlv := range p.Extensions {
		base := tlv.Type &^ TLVCriticalMask
		if knownTLV(base) && !allowed[base] {
			return invalid(ReasonMalformed, "extensions")
		}
		present[base] = true
	}
	for _, typ := range required {
		if !present[typ] {
			return invalid(ReasonMalformed, "extensions")
		}
	}
	return nil
}
func checkNumber(rule string, value uint64) error {
	if rule == "zero" && value != 0 {
		return fmt.Errorf("nonzero")
	}
	if rule == "nonzero" && value == 0 {
		return fmt.Errorf("zero")
	}
	return nil
}
