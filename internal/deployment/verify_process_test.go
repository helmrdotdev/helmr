package deployment

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestProgramVerifierResultRoundTrip(t *testing.T) {
	canonical := canonicalVerifierProgramIndex(t)
	tests := []struct {
		name       string
		write      func(*bytes.Buffer) error
		kind       programVerifierRecordKind
		index      []byte
		diagnostic string
	}{
		{
			name: "verified",
			write: func(output *bytes.Buffer) error {
				return writeProgramVerifierVerified(output, canonical)
			},
			kind:  programVerifierVerified,
			index: canonical,
		},
		{
			name: "invalid",
			write: func(output *bytes.Buffer) error {
				return writeProgramVerifierInvalid(output, "program index is missing")
			},
			kind:       programVerifierInvalid,
			diagnostic: "program index is missing",
		},
		{
			name: "failed",
			write: func(output *bytes.Buffer) error {
				return writeProgramVerifierFailed(output, "artifact read failed")
			},
			kind:       programVerifierFailed,
			diagnostic: "artifact read failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeProgramVerifierReady(&output); err != nil {
				t.Fatal(err)
			}
			if err := test.write(&output); err != nil {
				t.Fatal(err)
			}
			result, err := readProgramVerifierResult(bytes.NewReader(output.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if result.kind != test.kind ||
				!bytes.Equal(result.index, test.index) ||
				result.diagnostic != test.diagnostic {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestProgramVerifierResultRejectsMalformedOutput(t *testing.T) {
	canonical := canonicalVerifierProgramIndex(t)
	valid := func() []byte {
		var output bytes.Buffer
		if err := writeProgramVerifierReady(&output); err != nil {
			t.Fatal(err)
		}
		if err := writeProgramVerifierVerified(&output, canonical); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}()
	ready := recordBytes(programVerifierReady, nil)
	tests := map[string][]byte{
		"empty":             nil,
		"unknown readiness": recordBytes(0xff, nil),
		"readiness payload": recordBytes(programVerifierReady, []byte("x")),
		"missing terminal":  ready,
		"unknown terminal":  append(ready, recordBytes(0xff, []byte("x"))...),
		"empty terminal":    append(ready, recordBytes(programVerifierVerified, nil)...),
		"truncated header":  valid[:programVerifierHeaderBytes+2],
		"truncated payload": valid[:len(valid)-1],
		"trailing":          append(append([]byte(nil), valid...), 'x'),
		"duplicate terminal": append(
			append([]byte(nil), valid...),
			recordBytes(programVerifierVerified, canonical)...,
		),
		"verified before ready": recordBytes(programVerifierVerified, canonical),
	}
	var oversized [programVerifierHeaderBytes]byte
	oversized[0] = byte(programVerifierVerified)
	binary.BigEndian.PutUint32(oversized[1:], uint32(maxProgramFileSizeBytes+1))
	tests["oversized verified"] = append(append([]byte(nil), ready...), oversized[:]...)
	var oversizedDiagnostic [programVerifierHeaderBytes]byte
	oversizedDiagnostic[0] = byte(programVerifierInvalid)
	binary.BigEndian.PutUint32(
		oversizedDiagnostic[1:],
		uint32(programVerifierDiagnosticMaxBytes+1),
	)
	tests["oversized diagnostic"] = append(
		append([]byte(nil), ready...),
		oversizedDiagnostic[:]...,
	)
	tests["invalid UTF-8 diagnostic"] = append(
		append([]byte(nil), ready...),
		recordBytes(programVerifierInvalid, []byte{0xff})...,
	)
	tests["control character diagnostic"] = append(
		append([]byte(nil), ready...),
		recordBytes(programVerifierInvalid, []byte("first\nsecond"))...,
	)
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := readProgramVerifierResult(bytes.NewReader(input)); err == nil {
				t.Fatal("malformed result was accepted")
			}
		})
	}
}

func TestProgramVerifierResultValidatesPayloads(t *testing.T) {
	var output bytes.Buffer
	if err := writeProgramVerifierVerified(&output, []byte(`{"not":"a program index"}`)); err == nil ||
		!strings.Contains(err.Error(), "program verifier result") {
		t.Fatalf("verified error = %v", err)
	}
	for name, diagnostic := range map[string]string{
		"empty":       "",
		"control":     "first\nsecond",
		"invalidUTF8": string([]byte{0xff}),
		"oversized":   strings.Repeat("x", programVerifierDiagnosticMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeProgramVerifierInvalid(&output, diagnostic); err == nil {
				t.Fatal("invalid diagnostic was accepted")
			}
		})
	}
}

func recordBytes(kind programVerifierRecordKind, payload []byte) []byte {
	var output bytes.Buffer
	var header [programVerifierHeaderBytes]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	output.Write(header[:])
	output.Write(payload)
	return output.Bytes()
}

func canonicalVerifierProgramIndex(t *testing.T) []byte {
	t.Helper()
	canonical, err := CanonicalProgramIndex(ProgramIndex{
		FormatVersion:     ProgramIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
		RuntimeDigest:     "sha256:" + strings.Repeat("0", 64),
		Architecture:      ArchitectureX8664,
		Dependencies: ProgramDependencies{
			Digest:    "sha256:" + strings.Repeat("1", 64),
			SizeBytes: 1,
			MediaType: ProgramDependencyArtifactMediaType,
		},
		PackageGraph: ProgramFile{
			Digest:    "sha256:" + strings.Repeat("2", 64),
			SizeBytes: 1,
		},
		Modules: ProgramFile{
			Digest:    "sha256:" + strings.Repeat("3", 64),
			SizeBytes: 1,
		},
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "verify",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
