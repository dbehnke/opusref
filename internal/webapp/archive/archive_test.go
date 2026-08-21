package archive

import (
	"bytes"
	"os"
	"testing"

	"github.com/google/uuid"
)

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return w.Buffer.Write(p)
}

func TestArchivePreservesOpaquePackets(t *testing.T) {
	id := uuid.New()
	var output bytes.Buffer
	w, err := NewWriter(&output, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []Packet{{Sequence: 7, Timestamp: 960, ArrivalMS: 12, Payload: []byte{0xf8, 0xff, 0xfe}}, {Sequence: 8, Timestamp: 1920, ArrivalMS: 32, Payload: []byte{1}}}
	for _, p := range want {
		if err = w.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() != id {
		t.Fatalf("id=%v", r.ID())
	}
	for i := range want {
		got, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Sequence != want[i].Sequence || got.Timestamp != want[i].Timestamp || !bytes.Equal(got.Payload, want[i].Payload) {
			t.Fatalf("packet changed: %#v", got)
		}
	}
}

func TestReaderRejectsMalformedLengthAndReserved(t *testing.T) {
	id := uuid.New()
	var b bytes.Buffer
	w, _ := NewWriter(&b, id)
	_ = w.WritePacket(Packet{Payload: []byte{1}})
	for _, offset := range []int{24, 44} {
		data := append([]byte(nil), b.Bytes()...)
		data[offset] = 1
		if _, err := NewReader(bytes.NewReader(data), int64(len(data))); offset == 24 && err == nil {
			t.Fatal("reserved archive header accepted")
		}
		if offset == 44 {
			r, _ := NewReader(bytes.NewReader(data), int64(len(data)))
			if _, err := r.Next(); err == nil {
				t.Fatal("reserved entry accepted")
			}
		}
	}
}

func TestAtomicFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	id := uuid.New()
	f, err := CreateFile(dir, id, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Append(Packet{Payload: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	final, size, sum, err := f.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if size <= 32 || len(sum) != 32 {
		t.Fatal("bad final metadata")
	}
	if _, err = os.Stat(final); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(f.PartialPath()); !os.IsNotExist(err) {
		t.Fatal("partial file remains")
	}
}
func TestWriterCompletesShortWrites(t *testing.T) {
	var out shortWriter
	w, err := NewWriter(&out, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.WritePacket(Packet{Payload: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != HeaderSize+EntryHeaderSize+3 {
		t.Fatalf("size=%d", out.Len())
	}
}
