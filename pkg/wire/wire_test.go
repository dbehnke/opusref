package wire

import "testing"

func TestVersion1ConstantsMatchSpecification(t *testing.T) {
	tests := map[string]struct {
		got  int
		want int
	}{
		"base header size": {BaseHeaderSize, 32},
		"maximum datagram": {MaxDatagramSize, 1200},
		"callsign size":    {CallsignSize, 10},
		"timestamp rate":   {TimestampRate, 48000},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %d, want %d", test.got, test.want)
			}
		})
	}
}

func TestPacketTypeAllocations(t *testing.T) {
	tests := map[string]struct {
		got  PacketType
		want PacketType
	}{
		"hello":          {PacketHello, 0x01},
		"error":          {PacketError, 0x07},
		"stream request": {PacketStreamRequest, 0x20},
		"stream revoke":  {PacketStreamRevoke, 0x25},
		"audio":          {PacketAudio, 0x40},
		"data":           {PacketData, 0x41},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %#02x, want %#02x", test.got, test.want)
			}
		})
	}
}

func TestPublicAndPrivateDataTypeRangesDoNotOverlap(t *testing.T) {
	if DataTypeStandardMax >= DataTypePrivateMin {
		t.Fatalf("standard maximum %#04x overlaps private minimum %#04x", DataTypeStandardMax, DataTypePrivateMin)
	}
	if DataTypeStandardMin == 0 {
		t.Fatal("data type zero must stay invalid")
	}
}
