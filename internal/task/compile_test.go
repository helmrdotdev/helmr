package task

import (
	"bytes"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestDecodeTaskBundleResponseReturnsParseError(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.WriteParseErrorFrame(&buf, "task_not_found", "task not found: deploy"); err != nil {
		t.Fatal(err)
	}
	body, err := frameio.ReadMessageFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeTaskBundleResponse(body)
	var parseErr ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("err = %T %[1]v", err)
	}
	if parseErr.Kind != "task_not_found" || parseErr.Message != "task not found: deploy" {
		t.Fatalf("parse err = %+v", parseErr)
	}
}
