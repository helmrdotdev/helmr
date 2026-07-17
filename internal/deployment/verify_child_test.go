package deployment

import "testing"

func TestProgramVerifierChildModeRequiresExactPrivateArgument(t *testing.T) {
	if programVerifierExecutable != "/proc/self/exe" {
		t.Fatalf("child executable = %q", programVerifierExecutable)
	}
	for _, arguments := range [][]string{
		nil,
		{"helmr-worker"},
		{"helmr-worker", "status"},
		{"helmr-worker", programVerifierChildArgument, "extra"},
	} {
		handled, err := RunProgramVerifierChild(arguments)
		if handled || err != nil {
			t.Fatalf("RunProgramVerifierChild(%q) = (%t, %v), want (false, nil)", arguments, handled, err)
		}
	}

	arguments := programVerifierChildArguments()
	if len(arguments) != 1 || arguments[0] != programVerifierChildArgument {
		t.Fatalf("child arguments = %q", arguments)
	}
}
