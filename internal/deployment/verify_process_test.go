package deployment

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestProgramVerifierFrameRoundTrip(t *testing.T) {
	result := []byte(`{"verified":true}`)
	var framed bytes.Buffer
	if err := writeProgramVerifierFrame(&framed, result); err != nil {
		t.Fatal(err)
	}
	if got, err := readProgramVerifierFrame(bytes.NewReader(framed.Bytes())); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, result) {
		t.Fatalf("result = %q, want %q", got, result)
	}
}

func TestProgramVerifierFrameRejectsMalformedOutput(t *testing.T) {
	var oversized [programVerifierFrameBytes]byte
	binary.BigEndian.PutUint32(oversized[:], uint32(maxProgramFileSizeBytes+1))
	var valid bytes.Buffer
	if err := writeProgramVerifierFrame(&valid, []byte("result")); err != nil {
		t.Fatal(err)
	}
	validFrame := valid.Bytes()
	tests := map[string][]byte{
		"missing prefix": nil,
		"zero length":    make([]byte, programVerifierFrameBytes),
		"oversized":      oversized[:],
		"truncated":      validFrame[:len(validFrame)-1],
		"trailing":       append(append([]byte(nil), validFrame...), 'x'),
		"duplicate":      append(append([]byte(nil), validFrame...), validFrame...),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := readProgramVerifierFrame(bytes.NewReader(input)); err == nil {
				t.Fatal("malformed result was accepted")
			}
		})
	}
}

func TestProgramVerifierResultRequiresCanonicalProgramIndex(t *testing.T) {
	var output bytes.Buffer
	err := writeProgramVerifierResult(&output, []byte(`{"not":"a program index"}`))
	if err == nil || !strings.Contains(err.Error(), "program verifier result") {
		t.Fatalf("error = %v", err)
	}
}

func TestProgramVerifierResultRoundTrip(t *testing.T) {
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
	var output bytes.Buffer
	if err := writeProgramVerifierResult(&output, canonical); err != nil {
		t.Fatal(err)
	}
	result, err := readProgramVerifierResult(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, canonical) {
		t.Fatalf("result = %q, want %q", result, canonical)
	}
}
