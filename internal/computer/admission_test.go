package computer

import "testing"

// A zero Admission locked nothing and records no admission.
func TestZeroAdmissionRefusesTouch(t *testing.T) {
	if err := (Admission{}).Touch(t.Context()); err == nil {
		t.Fatal("zero Admission touched a Computer")
	}
}
