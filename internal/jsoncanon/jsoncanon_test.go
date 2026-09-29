package jsoncanon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type goldenFixture struct {
	Canonicalization []struct {
		Name      string `json:"name"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
	} `json:"canonicalization"`
	CanonicalRejections []struct {
		Name     string `json:"name"`
		InputHex string `json:"inputHex"`
	} `json:"canonicalRejections"`
}

func TestTransformPreservesTrailingSyntaxError(t *testing.T) {
	_, err := Transform([]byte(`{"a":1} }`))
	if err == nil {
		t.Fatal("Transform returned nil error")
	}
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("error = %v, want json.SyntaxError cause", err)
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("error = %q, want trailing data context", err.Error())
	}
}

func TestTransformReportsTruncatedInput(t *testing.T) {
	for _, raw := range []string{`{"a"`, `{"a":`, `{"a":1,"b":`, `"abc`, `tru`} {
		_, err := Transform([]byte(raw))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Transform(%q) error = %v, want unexpected EOF", raw, err)
		}
	}
	_, err := Transform([]byte("  \n"))
	if !errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Transform(whitespace) error = %v, want EOF", err)
	}
}

func TestTransformMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadGoldenFixture(t)
	for _, test := range fixture.Canonicalization {
		t.Run(test.Name, func(t *testing.T) {
			canonical, err := Transform([]byte(test.Input))
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != test.Canonical {
				t.Fatalf("canonical JSON = %q, want %q", canonical, test.Canonical)
			}
		})
	}
}

func TestTransformRejectsInvalidIJSON(t *testing.T) {
	fixture := loadGoldenFixture(t)
	for _, test := range fixture.CanonicalRejections {
		t.Run(test.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(test.InputHex)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Transform(raw); err == nil {
				t.Fatal("Transform returned nil error")
			}
		})
	}
}

func loadGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "tests", "fixtures", "contracts", "deployment-v0", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
