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

func TestProgramReceiptRejectsInvalidAuthority(t *testing.T) {
	base := testProgramReceipt(t)
	tests := map[string]func(*ProgramReceipt){
		"format version": func(receipt *ProgramReceipt) {
			receipt.FormatVersion = 1
		},
		"program digest": func(receipt *ProgramReceipt) {
			receipt.Program.Digest = "sha256:" + strings.Repeat("A", 64)
		},
		"program size": func(receipt *ProgramReceipt) {
			receipt.Program.SizeBytes = maxProgramPhysicalBytes + 1
		},
		"program media type": func(receipt *ProgramReceipt) {
			receipt.Program.MediaType = "application/octet-stream"
		},
		"program artifact ID": func(receipt *ProgramReceipt) {
			receipt.Program.ArtifactID = "invalid"
		},
		"index digest": func(receipt *ProgramReceipt) {
			receipt.Program.IndexDigest = "sha256:invalid"
		},
		"runtime": func(receipt *ProgramReceipt) {
			receipt.Runtime.APIVersion = "helmr.runtime.v1"
		},
		"source": func(receipt *ProgramReceipt) {
			receipt.Source.ArtifactID = "invalid"
		},
		"source size": func(receipt *ProgramReceipt) {
			receipt.Source.SizeBytes = maxJSONSafeInteger + 1
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := base
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

func TestProgramReceiptCanonicalSizeBound(t *testing.T) {
	if _, err := ParseProgramReceipt(make([]byte, maxProgramReceiptSizeBytes+1)); err == nil {
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
}

func testProgramReceipt(t *testing.T) ProgramReceipt {
	t.Helper()
	receipt, err := NewProgramReceipt(
		testProgramOutput(t),
		"019b635d-a915-7dca-8b86-26acc1007001",
		ProgramReceiptSource{
			ArtifactID: "019b635d-a915-7dca-8b86-26acc1007002",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			MediaType:  "application/vnd.helmr.deployment-source.v0+tar",
			SizeBytes:  1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func testProgramOutput(t *testing.T) ProgramOutput {
	t.Helper()
	var verified programVerification
	if err := json.Unmarshal(canonicalVerifierProgramVerification(t), &verified); err != nil {
		t.Fatal(err)
	}
	return ProgramOutput{
		Artifact: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("c", 64),
			SizeBytes: squashFSPhysicalAlign,
			MediaType: ProgramArtifactMediaType,
		},
		Index: verified.Index,
	}
}
