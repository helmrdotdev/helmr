package stablejson

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEncodePreservesTrailingSyntaxError(t *testing.T) {
	_, err := Encode([]byte(`{"a":1} }`))
	if err == nil {
		t.Fatal("Encode returned nil error")
	}
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("error = %v, want json.SyntaxError cause", err)
	}
	if !strings.Contains(err.Error(), "decode stable JSON trailing data") {
		t.Fatalf("error = %q, want trailing decode context", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("error = %q, want underlying syntax error", err.Error())
	}
}
