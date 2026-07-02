package frameio

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
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
	var buf bytes.Buffer
	buf.Write([]byte{0x10, 0x00, 0x00, 0x01})
	_, err := ReadMessageFrame(&buf)
	if err == nil {
		t.Fatal("expected oversized frame error")
	}
}

func TestProtoFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteProtoFrame(&buf, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	var event runv0.RunEvent
	if err := ReadProtoFrame(&buf, &event); err != nil {
		t.Fatal(err)
	}
	if event.GetTaskResult().GetExitCode() != 7 {
		t.Fatalf("event = %+v", event)
	}
}

func TestParseErrorFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteParseErrorFrame(&buf, "task_not_found", "task not found: deploy"); err != nil {
		t.Fatal(err)
	}
	body, err := ReadMessageFrame(&buf)
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
