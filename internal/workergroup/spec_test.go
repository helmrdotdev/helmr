package workergroup

import "testing"

func TestValidateName(t *testing.T) {
	if err := ValidateName("run-build"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRolesRequiresRole(t *testing.T) {
	if err := ValidateRoles(false, false); err == nil {
		t.Fatal("ValidateRoles() accepted a group without a role")
	}
}
