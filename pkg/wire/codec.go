package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

var (
	ErrMalformedPacket    = errors.New("malformed OpusRef packet")
	ErrUnsupportedVersion = errors.New("unsupported OpusRef version")
)

// TLV is one aligned header extension. Value does not include padding.
type TLV struct {
	Type  TLVType
	Value []byte
}

// Packet is one decoded OpusRef datagram.
type Packet struct {
	Header     Header
	Extensions []TLV
	Payload    []byte
}

// Encode returns the canonical datagram for packet.
func Encode(packet Packet) ([]byte, error) {
	extensionLength, err := validateExtensions(packet.Extensions)
	if err != nil {
		return nil, err
	}
	headerLength := BaseHeaderSize + extensionLength
	totalLength := headerLength + len(packet.Payload)
	if totalLength > MaxDatagramSize || len(packet.Payload) > int(^uint16(0)) {
		return nil, fmt.Errorf("%w: datagram length %d", ErrMalformedPacket, totalLength)
	}
	if packet.Header.Version != Version1 {
		return nil, ErrUnsupportedVersion
	}
	if packet.Header.Flags&FlagReservedMask != 0 {
		return nil, fmt.Errorf("%w: reserved flags are set", ErrMalformedPacket)
	}

	data := make([]byte, totalLength)
	copy(data[0:4], Magic)
	data[4] = packet.Header.Version
	data[5] = byte(packet.Header.Type)
	binary.BigEndian.PutUint16(data[6:8], packet.Header.Flags)
	binary.BigEndian.PutUint16(data[8:10], uint16(headerLength))
	binary.BigEndian.PutUint16(data[10:12], uint16(len(packet.Payload)))
	binary.BigEndian.PutUint64(data[12:20], packet.Header.SessionID)
	binary.BigEndian.PutUint32(data[20:24], packet.Header.StreamID)
	binary.BigEndian.PutUint32(data[24:28], packet.Header.Sequence)
	binary.BigEndian.PutUint32(data[28:32], packet.Header.Timestamp)

	offset := BaseHeaderSize
	for _, extension := range packet.Extensions {
		binary.BigEndian.PutUint16(data[offset:offset+2], uint16(extension.Type))
		binary.BigEndian.PutUint16(data[offset+2:offset+4], uint16(len(extension.Value)))
		copy(data[offset+4:], extension.Value)
		offset += alignedTLVSize(len(extension.Value))
	}
	copy(data[headerLength:], packet.Payload)
	return data, nil
}

// Decode validates and decodes one complete datagram.
func Decode(data []byte) (Packet, error) {
	if len(data) < BaseHeaderSize || len(data) > MaxDatagramSize {
		return Packet{}, fmt.Errorf("%w: datagram length %d", ErrMalformedPacket, len(data))
	}
	if string(data[0:4]) != Magic {
		return Packet{}, fmt.Errorf("%w: bad magic", ErrMalformedPacket)
	}
	if data[4] != Version1 {
		return Packet{}, ErrUnsupportedVersion
	}
	headerLength := int(binary.BigEndian.Uint16(data[8:10]))
	payloadLength := int(binary.BigEndian.Uint16(data[10:12]))
	if headerLength < BaseHeaderSize || headerLength%4 != 0 || headerLength > len(data) {
		return Packet{}, fmt.Errorf("%w: header length %d", ErrMalformedPacket, headerLength)
	}
	if headerLength+payloadLength != len(data) {
		return Packet{}, fmt.Errorf("%w: payload length %d", ErrMalformedPacket, payloadLength)
	}
	flags := binary.BigEndian.Uint16(data[6:8])
	if flags&FlagReservedMask != 0 {
		return Packet{}, fmt.Errorf("%w: reserved flags are set", ErrMalformedPacket)
	}

	packet := Packet{Header: Header{
		Version:       data[4],
		Type:          PacketType(data[5]),
		Flags:         flags,
		HeaderLength:  uint16(headerLength),
		PayloadLength: uint16(payloadLength),
		SessionID:     binary.BigEndian.Uint64(data[12:20]),
		StreamID:      binary.BigEndian.Uint32(data[20:24]),
		Sequence:      binary.BigEndian.Uint32(data[24:28]),
		Timestamp:     binary.BigEndian.Uint32(data[28:32]),
	}}

	seen := make(map[TLVType]struct{})
	for offset := BaseHeaderSize; offset < headerLength; {
		if headerLength-offset < 4 {
			return Packet{}, fmt.Errorf("%w: incomplete TLV", ErrMalformedPacket)
		}
		typeID := TLVType(binary.BigEndian.Uint16(data[offset : offset+2]))
		baseType := typeID &^ TLVCriticalMask
		valueLength := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		tlvSize := alignedTLVSize(valueLength)
		if tlvSize > headerLength-offset {
			return Packet{}, fmt.Errorf("%w: invalid TLV length", ErrMalformedPacket)
		}
		if _, exists := seen[baseType]; exists {
			return Packet{}, fmt.Errorf("%w: duplicate TLV %#04x", ErrMalformedPacket, typeID)
		}
		seen[baseType] = struct{}{}
		if typeID&TLVCriticalMask != 0 && !knownTLV(baseType) {
			return Packet{}, fmt.Errorf("%w: unknown critical TLV %#04x", ErrMalformedPacket, typeID)
		}
		valueEnd := offset + 4 + valueLength
		for _, padding := range data[valueEnd : offset+tlvSize] {
			if padding != 0 {
				return Packet{}, fmt.Errorf("%w: nonzero TLV padding", ErrMalformedPacket)
			}
		}
		value := append([]byte(nil), data[offset+4:valueEnd]...)
		if err := validateTLVValue(baseType, value); err != nil {
			return Packet{}, err
		}
		packet.Extensions = append(packet.Extensions, TLV{Type: typeID, Value: value})
		offset += tlvSize
	}
	packet.Payload = append([]byte(nil), data[headerLength:]...)
	return packet, nil
}

func validateExtensions(extensions []TLV) (int, error) {
	seen := make(map[TLVType]struct{}, len(extensions))
	total := 0
	for _, extension := range extensions {
		baseType := extension.Type &^ TLVCriticalMask
		if len(extension.Value) > int(^uint16(0)) {
			return 0, fmt.Errorf("%w: invalid TLV length", ErrMalformedPacket)
		}
		if _, exists := seen[baseType]; exists {
			return 0, fmt.Errorf("%w: duplicate TLV %#04x", ErrMalformedPacket, extension.Type)
		}
		seen[baseType] = struct{}{}
		if extension.Type&TLVCriticalMask != 0 && !knownTLV(baseType) {
			return 0, fmt.Errorf("%w: unknown critical TLV %#04x", ErrMalformedPacket, extension.Type)
		}
		if err := validateTLVValue(baseType, extension.Value); err != nil {
			return 0, err
		}
		total += alignedTLVSize(len(extension.Value))
	}
	return total, nil
}

func alignedTLVSize(valueLength int) int {
	return (4 + valueLength + 3) &^ 3
}

func knownTLV(typeID TLVType) bool {
	return typeID >= TLVTransactionID && typeID <= TLVEndReason
}

func validateTLVValue(typeID TLVType, value []byte) error {
	invalid := func() error {
		return fmt.Errorf("%w: invalid value for TLV %#04x", ErrMalformedPacket, typeID)
	}
	switch typeID {
	case TLVTransactionID:
		if len(value) != 8 {
			return invalid()
		}
	case TLVNodeCallsign, TLVSourceCallsign:
		if !validPaddedIdentifier(value, CallsignSize) {
			return invalid()
		}
	case TLVReflectorID:
		if !validPaddedIdentifier(value, 16) {
			return invalid()
		}
	case TLVDisplayName:
		if len(value) < 1 || len(value) > 64 || !utf8.Valid(value) {
			return invalid()
		}
	case TLVClientNonce, TLVServerNonce, TLVAuthenticationTag:
		if len(value) != 32 {
			return invalid()
		}
	case TLVDataType:
		if len(value) != 2 || binary.BigEndian.Uint16(value) == 0 {
			return invalid()
		}
	case TLVErrorCode:
		if len(value) != 2 {
			return invalid()
		}
		code := ErrorCode(binary.BigEndian.Uint16(value))
		if code < ErrorMalformedPacket || code > ErrorInternal {
			return invalid()
		}
	case TLVErrorText:
		if len(value) < 1 || len(value) > 128 || !utf8.Valid(value) {
			return invalid()
		}
	case TLVTransmitTimeLimit:
		if len(value) != 4 || binary.BigEndian.Uint32(value) == 0 {
			return invalid()
		}
	case TLVEndReason:
		if len(value) != 2 || EndReason(binary.BigEndian.Uint16(value)) > EndReasonServerShutdown {
			return invalid()
		}
	}
	return nil
}

func validPaddedIdentifier(value []byte, size int) bool {
	if len(value) != size {
		return false
	}
	contentLength := size
	for i, char := range value {
		if char == ' ' {
			contentLength = i
			break
		}
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '/' || char == '-') {
			return false
		}
	}
	if contentLength == 0 {
		return false
	}
	for _, char := range value[contentLength:] {
		if char != ' ' {
			return false
		}
	}
	return true
}
