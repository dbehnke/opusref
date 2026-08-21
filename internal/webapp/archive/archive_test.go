package archive

import (
	"bytes"
	"errors"
	"io"
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

func FuzzArchiveReader(f *testing.F) {
	var valid bytes.Buffer
	writer, _ := NewWriter(&valid, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	_ = writer.WritePacket(Packet{Sequence: 1, Timestamp: 960, ArrivalMS: 20, Payload: []byte{0xf8, 0xff}})
	f.Add(valid.Bytes())
	f.Add([]byte("ORAR"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		for {
			packet, nextErr := reader.Next()
			if nextErr != nil {
				if !errors.Is(nextErr, io.EOF) {
					return
				}
				return
			}
			if len(packet.Payload) < 1 || len(packet.Payload) > MaxPayload {
				t.Fatal("reader returned invalid payload length")
			}
		}
	})
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

func TestFileAndQuotaLimitsReturnDistinctErrors(t *testing.T) {
	id := uuid.New()
	fileLimited, err := createFile(t.TempDir(), id, 1024, HeaderSize+EntryHeaderSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if err = fileLimited.Append(Packet{Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err = fileLimited.Append(Packet{Payload: []byte{2}}); !errors.Is(err, ErrFileLimit) || errors.Is(err, ErrQuotaLimit) {
		t.Fatalf("file limit error=%v", err)
	}
	if _, _, _, err = fileLimited.Finalize(); err != nil {
		t.Fatal(err)
	}
	quotaLimited, err := createFile(t.TempDir(), uuid.New(), HeaderSize+EntryHeaderSize+1, MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	if err = quotaLimited.Append(Packet{Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err = quotaLimited.Append(Packet{Payload: []byte{2}}); !errors.Is(err, ErrQuotaLimit) || errors.Is(err, ErrFileLimit) {
		t.Fatalf("quota limit error=%v", err)
	}
	if _, _, _, err = quotaLimited.Finalize(); err != nil {
		t.Fatal(err)
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
