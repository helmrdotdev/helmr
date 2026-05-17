package guest

import (
	"bytes"
	"io"
	"testing"
)

func TestStreamFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	hash := "sha256:abc"
	if err := WriteStreamFrame(&buf, StreamHeader{
		Type:        StreamTypeRunImage,
		RunID:       "run-1",
		ContentHash: &hash,
	}, []byte("tar")); err != nil {
		t.Fatal(err)
	}
	header, bodyLen, err := ReadStreamFrameHeader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(&buf, int64(bodyLen)))
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != StreamTypeRunImage || header.RunID != "run-1" || header.ContentHash == nil || *header.ContentHash != hash || string(body) != "tar" {
		t.Fatalf("header = %+v body = %q", header, body)
	}
}
