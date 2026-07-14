package workergroup

import "testing"

func TestNormalizeSpec(t *testing.T) {
	spec, err := Normalize(Spec{ID: " group-1 ", AllowsRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != "group-1" || spec.Name != "group-1" || !spec.AllowsRun || spec.AllowsBuild {
		t.Fatalf("Normalize() = %#v", spec)
	}
}

func TestNormalizeSpecRequiresRole(t *testing.T) {
	if _, err := Normalize(Spec{ID: "group-1"}); err == nil {
		t.Fatal("Normalize() accepted a group without a role")
	}
}
