package wire

import (
	"encoding/binary"
	"errors"
	"strings"
)

func Find(p Packet, typ TLVType) ([]byte, bool) {
	for _, tlv := range p.Extensions {
		if tlv.Type&^TLVCriticalMask == typ {
			return append([]byte(nil), tlv.Value...), true
		}
	}
	return nil, false
}
func Uint64TLV(typ TLVType, value uint64) TLV {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, value)
	return TLV{Type: typ, Value: data}
}
func Uint32TLV(typ TLVType, value uint32) TLV {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, value)
	return TLV{Type: typ, Value: data}
}
func Uint16TLV(typ TLVType, value uint16) TLV {
	data := make([]byte, 2)
	binary.BigEndian.PutUint16(data, value)
	return TLV{Type: typ, Value: data}
}
func Callsign(value string) ([]byte, error) {
	value = strings.ToUpper(value)
	if len(value) < 1 || len(value) > CallsignSize {
		return nil, errors.New("callsign length must be 1 through 10")
	}
	data := []byte(value + strings.Repeat(" ", CallsignSize-len(value)))
	if !validPaddedIdentifier(data, CallsignSize) {
		return nil, errors.New("callsign contains an invalid character")
	}
	return data, nil
}
func ReflectorID(value string) ([]byte, error) {
	value = strings.ToUpper(value)
	if len(value) < 1 || len(value) > 16 {
		return nil, errors.New("reflector ID length must be 1 through 16")
	}
	data := []byte(value + strings.Repeat(" ", 16-len(value)))
	if !validPaddedIdentifier(data, 16) {
		return nil, errors.New("reflector ID contains an invalid character")
	}
	return data, nil
}
