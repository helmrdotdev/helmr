package workergroup

import "testing"

func TestValidateName(t *testing.T) {
	if err := ValidateName("run-build"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Run", "run_2", "-run", "run-"} {
		if err := ValidateName(name); err == nil {
			t.Fatalf("ValidateName(%q) succeeded", name)
		}
	}
}
