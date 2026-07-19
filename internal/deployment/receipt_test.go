package deployment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestProgramReceiptRoundTrip(t *testing.T) {
	receipt := testProgramReceipt(t)
	canonical, err := CanonicalProgramReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramReceipt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, receipt) {
		t.Fatalf("parsed receipt = %#v, want %#v", parsed, receipt)
	}
}

func TestProgramReceiptRejectsIncompleteOrDivergentShape(t *testing.T) {
	base := testProgramReceipt(t)
	tests := map[string]func(*ProgramReceipt){
		"format version": func(receipt *ProgramReceipt) {
			receipt.FormatVersion = 1
		},
		"code digest": func(receipt *ProgramReceipt) {
			receipt.Code.Digest = "sha256:" + strings.Repeat("A", 64)
		},
		"code size": func(receipt *ProgramReceipt) {
			receipt.Code.SizeBytes = maxCodePhysicalBytes + 1
		},
		"code media type": func(receipt *ProgramReceipt) {
			receipt.Code.MediaType = ProgramDependencyArtifactMediaType
		},
		"dependency size": func(receipt *ProgramReceipt) {
			receipt.Dependencies.SizeBytes = maxDependencyPhysicalBytes + 1
		},
		"dependency mismatch": func(receipt *ProgramReceipt) {
			receipt.Index.Dependencies.Digest = "sha256:" + strings.Repeat("d", 64)
		},
		"dependency index runtime": func(receipt *ProgramReceipt) {
			receipt.DependencyIndex.RuntimeDigest = "sha256:" + strings.Repeat("d", 64)
		},
		"dependency index package graph": func(receipt *ProgramReceipt) {
			receipt.DependencyIndex.PackageGraphDigest = "sha256:" + strings.Repeat("d", 64)
		},
		"index": func(receipt *ProgramReceipt) {
			receipt.Index.RuntimeAPIVersion = "helmr.runtime.v1"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := cloneProgramReceipt(base)
			mutate(&receipt)
			if err := ValidateProgramReceipt(receipt); err == nil {
				t.Fatal("ValidateProgramReceipt returned nil error")
			}
			if _, err := CanonicalProgramReceipt(receipt); err == nil {
				t.Fatal("CanonicalProgramReceipt returned nil error")
			}
		})
	}
}

func TestProgramReceiptParserRequiresClosedCanonicalObject(t *testing.T) {
	canonical, err := CanonicalProgramReceipt(testProgramReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"unknown":      append(canonical[:len(canonical)-1], []byte(`,"unknown":true}`)...),
		"duplicate": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0`,
			`"formatVersion":0,"formatVersion":0`,
			1,
		)),
		"empty": nil,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProgramReceipt(raw); err == nil {
				t.Fatal("ParseProgramReceipt returned nil error")
			}
		})
	}
}

func TestProgramReceiptParserReturnsDefensiveDeclarations(t *testing.T) {
	canonical, err := CanonicalProgramReceipt(testProgramReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ParseProgramReceipt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Index.Declarations[0].Slots[0] = DeclarationSlotSchema
	again, err := ParseProgramReceipt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if again.Index.Declarations[0].Slots[0] != DeclarationSlotHandler {
		t.Fatal("parsed receipt retained caller mutation")
	}
}

func TestProgramReceiptCanonicalSizeBound(t *testing.T) {
	raw := make([]byte, maxProgramReceiptSizeBytes+1)
	if _, err := ParseProgramReceipt(raw); err == nil {
		t.Fatal("ParseProgramReceipt accepted oversized input")
	}
}

func TestProgramVerificationRoundTrip(t *testing.T) {
	canonical := canonicalVerifierProgramVerification(t)
	verified, err := parseProgramVerification(canonical)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := canonicalProgramVerification(verified)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reencoded, canonical) {
		t.Fatalf("reencoded verification = %q, want %q", reencoded, canonical)
	}
	verified.Index.Declarations[0].Slots[0] = DeclarationSlotSchema
	again, err := parseProgramVerification(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if again.Index.Declarations[0].Slots[0] != DeclarationSlotHandler {
		t.Fatal("parsed verification retained caller mutation")
	}
}

func TestProgramVerificationRejectsDivergentIndexes(t *testing.T) {
	verified, err := parseProgramVerification(canonicalVerifierProgramVerification(t))
	if err != nil {
		t.Fatal(err)
	}
	verified.DependencyIndex.PackageGraphSizeBytes++
	if _, err := canonicalProgramVerification(verified); err == nil {
		t.Fatal("canonicalProgramVerification accepted divergent package graph identity")
	}
}

func testProgramReceipt(t *testing.T) ProgramReceipt {
	t.Helper()
	var verified programVerification
	if err := json.Unmarshal(canonicalVerifierProgramVerification(t), &verified); err != nil {
		t.Fatal(err)
	}
	dependencies := verified.Index.Dependencies
	return ProgramReceipt{
		FormatVersion:   ProgramReceiptFormatVersion,
		DependencyIndex: verified.DependencyIndex,
		Code: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("c", 64),
			SizeBytes: squashFSPhysicalAlign,
			MediaType: ProgramCodeArtifactMediaType,
		},
		Dependencies: dependencies,
		Index:        verified.Index,
	}
}
