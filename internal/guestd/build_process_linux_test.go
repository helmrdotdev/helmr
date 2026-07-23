//go:build linux

package guestd

import (
	"bytes"
	"testing"
)

func TestBuildOutputRetainsOneCombinedBound(t *testing.T) {
	output := &buildOutput{limit: 8}
	output.append([]byte("12345"), true)
	output.append([]byte("678"), false)
	result := output.result(nil)
	if result.overflow {
		t.Fatal("output at the exact bound was marked truncated")
	}
	if got := append(result.stdout, result.stderr...); !bytes.Equal(
		got,
		[]byte("12345678"),
	) {
		t.Fatalf("retained output = %q", got)
	}
}

func TestBuildOutputDrainsAndMarksTruncation(t *testing.T) {
	limit := len(buildOutputMarker) + 4
	output := &buildOutput{limit: limit}
	output.append([]byte("stdout"), true)
	output.append([]byte("stderr"), false)
	result := output.result(nil)
	if !result.overflow {
		t.Fatal("output above the bound was not marked truncated")
	}
	combined := append(
		append([]byte(nil), result.stdout...),
		result.stderr...,
	)
	if len(combined) != limit {
		t.Fatalf("retained output size = %d, want %d", len(combined), limit)
	}
	if !bytes.HasSuffix(combined, []byte(buildOutputMarker)) {
		t.Fatalf("retained output lacks truncation marker: %q", combined)
	}
}
