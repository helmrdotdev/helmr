package workerpoolname

import "testing"

func TestValidate(t *testing.T) {
	if err := Validate("run-v2"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Run", "run_2", "-run", "run-"} {
		if err := Validate(name); err == nil {
			t.Fatalf("Validate(%q) succeeded", name)
		}
	}
}
