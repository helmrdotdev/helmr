package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeIndexRoundTrip(t *testing.T) {
	index := RuntimeIndex{
		Architecture:      ArchitectureX8664,
		FormatVersion:     RuntimeIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
	}
	canonical, err := CanonicalRuntimeIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"architecture":"x86_64","formatVersion":0,"runtimeApiVersion":"helmr.runtime.v0"}`
	if string(canonical) != want {
		t.Fatalf("canonical runtime index = %q, want %q", canonical, want)
	}
	parsed, err := ParseRuntimeIndex(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != index {
		t.Fatalf("parsed runtime index = %#v, want %#v", parsed, index)
	}
}

func TestRuntimeDescriptorRoundTrip(t *testing.T) {
	descriptor := testRuntimeDescriptor()
	canonical, err := CanonicalRuntimeDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"architecture":"x86_64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeApiVersion":"helmr.runtime.v0","sizeBytes":4096}`
	if string(canonical) != want {
		t.Fatalf("canonical runtime descriptor = %q, want %q", canonical, want)
	}
	parsed, err := ParseRuntimeDescriptor(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, descriptor) {
		t.Fatalf("parsed runtime descriptor = %#v, want %#v", parsed, descriptor)
	}
}

func TestRuntimeDescriptorDomainIsIndependentFromArtifactAdmission(t *testing.T) {
	descriptor := testRuntimeDescriptor()
	descriptor.SizeBytes = maxRuntimePhysicalBytes + 1
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		t.Fatalf("descriptor scalar domain rejected physical oversize: %v", err)
	}
	if _, err := SnapshotRuntimeArtifact(
		context.Background(),
		t.TempDir(),
		descriptor,
		bytes.NewReader(nil),
	); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("runtime Artifact snapshot error = %v", err)
	}
}

func TestRuntimeDocumentsRejectIncompleteOrDivergentShape(t *testing.T) {
	index := RuntimeIndex{
		Architecture:      ArchitectureX8664,
		FormatVersion:     RuntimeIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
	}
	indexTests := map[string]func(*RuntimeIndex){
		"format version": func(value *RuntimeIndex) { value.FormatVersion = 1 },
		"runtime API":    func(value *RuntimeIndex) { value.RuntimeAPIVersion = "helmr.runtime.v1" },
		"architecture":   func(value *RuntimeIndex) { value.Architecture = "amd64" },
	}
	for name, mutate := range indexTests {
		t.Run("index "+name, func(t *testing.T) {
			value := index
			mutate(&value)
			if err := ValidateRuntimeIndex(value); err == nil {
				t.Fatal("ValidateRuntimeIndex returned nil error")
			}
		})
	}

	descriptor := testRuntimeDescriptor()
	descriptorTests := map[string]func(*RuntimeDescriptor){
		"format version": func(value *RuntimeDescriptor) { value.FormatVersion = 1 },
		"architecture":   func(value *RuntimeDescriptor) { value.Architecture = "amd64" },
		"digest":         func(value *RuntimeDescriptor) { value.Digest = "sha256:invalid" },
		"media type":     func(value *RuntimeDescriptor) { value.MediaType += "; charset=binary" },
		"runtime API":    func(value *RuntimeDescriptor) { value.RuntimeAPIVersion = "helmr.runtime.v1" },
		"zero size":      func(value *RuntimeDescriptor) { value.SizeBytes = 0 },
		"oversize":       func(value *RuntimeDescriptor) { value.SizeBytes = maxJSONSafeInteger + 1 },
	}
	for name, mutate := range descriptorTests {
		t.Run("descriptor "+name, func(t *testing.T) {
			value := descriptor
			mutate(&value)
			if err := ValidateRuntimeDescriptor(value); err == nil {
				t.Fatal("ValidateRuntimeDescriptor returned nil error")
			}
		})
	}
}

func TestRuntimeDocumentParsersRequireClosedCanonicalObjects(t *testing.T) {
	index, err := CanonicalRuntimeIndex(RuntimeIndex{
		Architecture:      ArchitectureX8664,
		FormatVersion:     RuntimeIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := CanonicalRuntimeDescriptor(testRuntimeDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		raw   []byte
		parse func([]byte) error
	}{
		"index noncanonical": {
			raw: append([]byte(" "), index...),
			parse: func(raw []byte) error {
				_, err := ParseRuntimeIndex(raw)
				return err
			},
		},
		"index unknown": {
			raw: append(index[:len(index)-1], []byte(`,"unknown":true}`)...),
			parse: func(raw []byte) error {
				_, err := ParseRuntimeIndex(raw)
				return err
			},
		},
		"descriptor duplicate": {
			raw: []byte(strings.Replace(
				string(descriptor),
				`"formatVersion":0`,
				`"formatVersion":0,"formatVersion":0`,
				1,
			)),
			parse: func(raw []byte) error {
				_, err := ParseRuntimeDescriptor(raw)
				return err
			},
		},
		"descriptor fractional size": {
			raw: func() []byte {
				var value map[string]any
				if err := json.Unmarshal(descriptor, &value); err != nil {
					t.Fatal(err)
				}
				value["sizeBytes"] = 1.5
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}(),
			parse: func(raw []byte) error {
				_, err := ParseRuntimeDescriptor(raw)
				return err
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := test.parse(test.raw); err == nil {
				t.Fatal("parser returned nil error")
			}
		})
	}
}

func testRuntimeDescriptor() RuntimeDescriptor {
	return RuntimeDescriptor{
		Architecture:      ArchitectureX8664,
		Digest:            "sha256:" + strings.Repeat("a", 64),
		FormatVersion:     RuntimeDescriptorFormatVersion,
		MediaType:         RuntimeArtifactMediaType,
		RuntimeAPIVersion: RuntimeAPIVersion,
		SizeBytes:         squashFSPhysicalAlign,
	}
}
