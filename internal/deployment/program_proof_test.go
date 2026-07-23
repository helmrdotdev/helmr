package deployment

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

func TestProgramProofRoundTripAndExactFrame(t *testing.T) {
	result := ProgramProofResult{
		FormatVersion: ProgramProofFormatVersion,
		Outcome:       ProgramProofSucceeded,
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "send-email",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
	}
	raw, err := CanonicalProgramProofResult(result)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramProofResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !sameProgramDeclaration(
		parsed.Declarations[0],
		result.Declarations[0],
	) {
		t.Fatalf("Program proof = %+v", parsed)
	}
	var framed bytes.Buffer
	if err := frameio.WriteMessageFrame(&framed, raw); err != nil {
		t.Fatal(err)
	}
	framed.WriteByte(0)
	if _, err := ReadProgramProofFrame(&framed); err == nil {
		t.Fatal("Program proof frame with trailing data was accepted")
	}
}

func TestProgramProofRejectsIncompleteShapes(t *testing.T) {
	tests := []ProgramProofResult{
		{
			FormatVersion: ProgramProofFormatVersion,
			Outcome:       ProgramProofSucceeded,
		},
		{
			FormatVersion: ProgramProofFormatVersion,
			Outcome:       ProgramProofFailed,
			Error: &ProgramProofError{
				Reason:  "other",
				Message: "invalid",
			},
		},
		{
			FormatVersion: ProgramProofFormatVersion,
			Outcome:       ProgramProofFailed,
			Declarations:  []ProgramDeclaration{},
			Error: &ProgramProofError{
				Reason:  ProgramProofFailureReason,
				Message: "invalid",
			},
		},
	}
	for index, result := range tests {
		if _, err := CanonicalProgramProofResult(result); err == nil {
			t.Fatalf("invalid Program proof %d was accepted", index)
		}
	}
}
