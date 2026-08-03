package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProgramBuildInputCleanupFailureIsNotAnInputFailure(t *testing.T) {
	cause := &ProgramBuildInputFailure{
		Reason: BuildFailureInvalidSource,
		err:    errors.New("invalid source"),
	}

	result := closeProgramBuildInputsAfterError(cleanupFailingProgramBuildInputs(t), cause)
	var inputFailure *ProgramBuildInputFailure
	if errors.As(result, &inputFailure) {
		t.Fatalf("cleanup error retained input-failure classification: %v", result)
	}
	if result == nil || errors.Is(result, cause) {
		t.Fatalf("cleanup error did not replace input failure: %v", result)
	}
}

func TestProgramBuildInputCleanupFailurePreservesInfrastructureError(t *testing.T) {
	cause := errors.New("read CAS")
	result := closeProgramBuildInputsAfterError(cleanupFailingProgramBuildInputs(t), cause)
	if !errors.Is(result, cause) {
		t.Fatalf("cleanup error lost infrastructure cause: %v", result)
	}
}

func cleanupFailingProgramBuildInputs(t *testing.T) *ProgramBuildInputs {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "source-")
	if err != nil {
		t.Fatal(err)
	}
	nonempty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonempty, "retained"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &ProgramBuildInputs{source: &submittedSource{File: file, path: nonempty}}
}
