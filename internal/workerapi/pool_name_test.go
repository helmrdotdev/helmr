package workerapi

import "testing"

func TestValidatePoolName(t *testing.T) {
	if err := ValidatePoolName("run-v2"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Run", "run_2", "-run", "run-"} {
		if err := ValidatePoolName(name); err == nil {
			t.Fatalf("ValidatePoolName(%q) succeeded", name)
		}
	}
}
