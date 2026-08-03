package guestd

import "testing"

func TestProgramCgroupLeafIsStableAndProgramSpecific(t *testing.T) {
	first, err := programCgroupLeafName("run-1", 2, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := programCgroupLeafName("run-1", 2, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := programCgroupLeafName("run-2", 2, "lease-2")
	if err != nil {
		t.Fatal(err)
	}
	if first != replay || first == second {
		t.Fatalf("Program cgroup leaves = %q, %q, %q", first, replay, second)
	}
	if err := validateProgramCgroupLeaf(first); err != nil {
		t.Fatal(err)
	}
}

func TestProgramCgroupLeafRejectsIncompleteOrUntrustedNames(t *testing.T) {
	if _, err := programCgroupLeafName("", 1, "lease-1"); err == nil {
		t.Fatal("empty Run ID was accepted")
	}
	if _, err := programCgroupLeafName("run-1", 0, "lease-1"); err == nil {
		t.Fatal("zero attempt was accepted")
	}
	for _, leaf := range []string{"run", "../run-deadbeef", "run-not-hex"} {
		if err := validateProgramCgroupLeaf(leaf); err == nil {
			t.Fatalf("untrusted cgroup leaf %q was accepted", leaf)
		}
	}
}
