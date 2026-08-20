// Package wire defines the public OpusRef v1 wire vocabulary.
//
// The bootstrap implements structural encoding and decoding. Packet-specific
// semantic validation remains part of later protocol implementation.
package wire

const (
	Magic                 = "OPRF"
	Version1        uint8 = 1
	BaseHeaderSize        = 32
	MaxDatagramSize       = 1200
	CallsignSize          = 10
	TimestampRate         = 48000
)

const (
	FlagResponse uint16 = 1 << iota
	FlagRetry
	FlagReservedMask = 0xfffc
)

// PacketType identifies an OpusRef datagram.
type PacketType uint8

const (
	PacketHello PacketType = 0x01 + iota
	PacketChallenge
	PacketAuthenticate
	PacketWelcome
	PacketKeepalive
	PacketDisconnect
	PacketError
)

const (
	PacketStreamRequest PacketType = 0x20 + iota
	PacketStreamGrant
	PacketStreamBusy
	PacketStreamStart
	PacketStreamEnd
	PacketStreamRevoke
)

const (
	PacketAudio PacketType = 0x40 + iota
	PacketData
)

// Header is the logical form of the 32-byte base header. Implementations must
// encode it in network byte order as specified in docs/protocol-v1.md.
type Header struct {
	Version       uint8
	Type          PacketType
	Flags         uint16
	HeaderLength  uint16
	PayloadLength uint16
	SessionID     uint64
	StreamID      uint32
	Sequence      uint32
	Timestamp     uint32
}

// TLVType identifies a header extension. Bit 15 marks a critical extension.
type TLVType uint16

const TLVCriticalMask TLVType = 0x8000

const (
	TLVTransactionID TLVType = 0x0001 + iota
	TLVNodeCallsign
	TLVSourceCallsign
	TLVReflectorID
	TLVDisplayName
	TLVClientNonce
	TLVServerNonce
	TLVAuthenticationTag
	TLVDataType
	TLVErrorCode
	TLVErrorText
	TLVTransmitTimeLimit
	TLVEndReason
)

// ErrorCode identifies a protocol error without exposing implementation text.
type ErrorCode uint16

const (
	ErrorMalformedPacket ErrorCode = 1 + iota
	ErrorUnsupportedVersion
	ErrorAuthenticationFailed
	ErrorInvalidSession
	ErrorInvalidStream
	ErrorLimitExceeded
	ErrorUnsupportedType
	ErrorInternal
)

// EndReason identifies why the server released the floor.
type EndReason uint16

const (
	EndReasonNormal EndReason = iota
	EndReasonOwnerDisconnect
	EndReasonGrantTimeout
	EndReasonMediaInactivity
	EndReasonTransmitTimeLimit
	EndReasonServerShutdown
)

const (
	DataTypeStandardMin uint16 = 0x0001
	DataTypeStandardMax uint16 = 0x7fff
	DataTypePrivateMin  uint16 = 0x8000
	DataTypePrivateMax  uint16 = 0xffff
)
