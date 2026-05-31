package transport

import (
	"bytes"
	"io"
	"testing"
)

func TestStreamFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	hash := "sha256:abc"
	body := []byte("tar")
	if err := WriteStreamFrameHeader(&buf, StreamHeader{
		Type:       StreamTypeRunImage,
		RunID:      "run-1",
		BodyDigest: &hash,
	}, uint64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write(body); err != nil {
		t.Fatal(err)
	}
	header, bodyLen, err := ReadStreamFrameHeader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, err := io.ReadAll(io.LimitReader(&buf, int64(bodyLen)))
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != StreamTypeRunImage || header.RunID != "run-1" || header.BodyDigest == nil || *header.BodyDigest != hash || string(gotBody) != "tar" {
		t.Fatalf("header = %+v body = %q", header, gotBody)
	}
}

func TestStreamFrameHeaderSupportsLargeBodies(t *testing.T) {
	var buf bytes.Buffer
	const bodyLen = uint64(1 << 32)
	if err := WriteStreamFrameHeader(&buf, StreamHeader{Type: StreamTypeRunImage, RunID: "run-1"}, bodyLen); err != nil {
		t.Fatal(err)
	}
	_, gotBodyLen, err := ReadStreamFrameHeader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if gotBodyLen != bodyLen {
		t.Fatalf("bodyLen = %d, want %d", gotBodyLen, bodyLen)
	}
}
