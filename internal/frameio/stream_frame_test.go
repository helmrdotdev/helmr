package frameio

import (
	"bytes"
	"math"
	"testing"
)

func TestReadStreamFrameHeaderBoundedRejectsBeforeAllocation(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStreamFrameHeader(&buf, []byte("header"), 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStreamFrameHeaderBounded(&buf, 5, 8); err == nil {
		t.Fatal("expected header bound error")
	}

	buf.Reset()
	if err := WriteStreamFrameHeader(&buf, []byte("header"), 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStreamFrameHeaderBounded(&buf, 7, 7); err == nil {
		t.Fatal("expected body bound error")
	}
}

func TestWriteStreamFrameHeaderRejectsLengthOverflow(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStreamFrameHeader(&buf, []byte("x"), math.MaxUint64); err == nil {
		t.Fatal("expected stream frame length overflow")
	}
	if buf.Len() != 0 {
		t.Fatal("overflowing stream frame wrote output")
	}
}
