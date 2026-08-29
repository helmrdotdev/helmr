package deployment

import "testing"

func TestVerifierChildModeRequiresExactPrivateArguments(t *testing.T) {
	if verifierExecutable != "/proc/self/exe" {
		t.Fatalf("child executable = %q", verifierExecutable)
	}
	for _, arguments := range [][]string{
		nil,
		{"worker"},
		{"worker", "status"},
	} {
		handled, err := RunVerifierChild(arguments)
		if handled || err != nil {
			t.Fatalf("RunVerifierChild(%q) = (%t, %v), want (false, nil)", arguments, handled, err)
		}
	}
	for _, arguments := range [][]string{
		{"worker", verifierChildArgument},
		{"worker", verifierChildArgument, "unknown"},
		{"worker", verifierChildArgument, "program", "extra"},
	} {
		handled, err := RunVerifierChild(arguments)
		if !handled || err == nil {
			t.Fatalf("RunVerifierChild(%q) = (%t, %v), want handled error", arguments, handled, err)
		}
	}
	for _, job := range []verifierJob{programVerifierJob, runtimeVerifierJob} {
		arguments := verifierChildArguments(job)
		if len(arguments) != 2 ||
			arguments[0] != verifierChildArgument ||
			arguments[1] != string(job) {
			t.Fatalf("%s child arguments = %q", job, arguments)
		}
	}
}
