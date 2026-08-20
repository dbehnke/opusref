package record

import (
	"bytes"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestGoldenRecord(t *testing.T) {
	want, _ := hex.DecodeString("4f525231010000000102030400000003f8fffe")
	var b bytes.Buffer
	if err := Write(&b, Record{Kind: KindAudio, Timestamp: 0x01020304, Payload: []byte{0xf8, 0xff, 0xfe}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("got %x", b.Bytes())
	}
	got, err := Read(&b)
	if err != nil || got.Timestamp != 0x01020304 || !bytes.Equal(got.Payload, []byte{0xf8, 0xff, 0xfe}) {
		t.Fatalf("%#v %v", got, err)
	}
}
func TestReadRejectsMalformedRecordsBeforePayloadAllocation(t *testing.T) {
	tests := [][]byte{[]byte("short"), make([]byte, 16), func() []byte { b := make([]byte, 16); copy(b, "ORR1"); b[4] = 1; b[15] = 1; return b }(), func() []byte { b := make([]byte, 16); copy(b, "ORR1"); b[4] = 1; b[12] = 1; return b }()}
	for _, data := range tests {
		if _, err := Read(bytes.NewReader(data)); err == nil {
			t.Fatalf("accepted %x", data)
		}
	}
	if _, err := Read(strings.NewReader("")); err != io.EOF {
		t.Fatalf("got %v", err)
	}
}
