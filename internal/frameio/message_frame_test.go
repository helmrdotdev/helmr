package frameio

import (
	"bytes"
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
	var buf bytes.Buffer
	buf.Write([]byte{0x10, 0x00, 0x00, 0x01})
	_, err := ReadMessageFrame(&buf)
	if err == nil {
		t.Fatal("expected oversized frame error")
	}
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
