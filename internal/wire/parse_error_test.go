package wire

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

func TestParseErrorFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteParseErrorFrame(&buf, "task_not_found", "task not found: deploy"); err != nil {
		t.Fatal(err)
	}
	body, err := frameio.ReadMessageFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	frame, ok, err := DecodeParseErrorFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected parse error frame")
	}
	if frame.Kind != "task_not_found" || frame.Message != "task not found: deploy" {
		t.Fatalf("frame = %+v", frame)
	}
}

func TestDecodeParseErrorFrameIgnoresNonJSONBody(t *testing.T) {
	_, ok, err := DecodeParseErrorFrame([]byte{0x01, 0x00, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected non-json body to be ignored")
	}
}
