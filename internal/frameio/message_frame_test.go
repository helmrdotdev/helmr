package frameio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestMessageFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessageFrame(&buf, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	body, err := ReadMessageFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestReadMessageFrameRejectsOversizedBody(t *testing.T) {
	assertFrameRejectedBeforeBodyRead(t, ReadMessageFrame, MaxFrameBytes+1, MaxFrameBytes)
}

func TestReadMessageFrameBoundedRejectsBeforeBodyRead(t *testing.T) {
	assertFrameRejectedBeforeBodyRead(t, func(reader io.Reader) ([]byte, error) {
		return ReadMessageFrameBounded(reader, 4)
	}, 5, 4)
}

func assertFrameRejectedBeforeBodyRead(t *testing.T, read func(io.Reader) ([]byte, error), size, limit uint32) {
	t.Helper()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], size)
	reader := &frameBodyReadSentinel{header: bytes.NewReader(header[:])}
	_, err := read(reader)
	want := fmt.Sprintf("frameio message frame length %d exceeds max %d", size, limit)
	if err == nil || err.Error() != want {
		t.Errorf("frame error = %v, want %q", err, want)
	}
	if reader.bodyReads != 0 {
		t.Errorf("oversized frame attempted %d body reads", reader.bodyReads)
	}
	if reader.header.Len() != 0 {
		t.Errorf("reader retained %d header bytes", reader.header.Len())
	}
}

type frameBodyReadSentinel struct {
	header    *bytes.Reader
	bodyReads int
}

func (r *frameBodyReadSentinel) Read(p []byte) (int, error) {
	if r.header.Len() != 0 {
		return r.header.Read(p)
	}
	r.bodyReads++
	return 0, io.EOF
}

func TestProtoFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteProtoFrame(&buf, wrapperspb.Int32(7)); err != nil {
		t.Fatal(err)
	}
	var value wrapperspb.Int32Value
	if err := ReadProtoFrame(&buf, &value); err != nil {
		t.Fatal(err)
	}
	if value.GetValue() != 7 {
		t.Fatalf("value = %d", value.GetValue())
	}
}
