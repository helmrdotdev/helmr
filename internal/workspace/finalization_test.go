package workspace

import "testing"

func TestFinalizationFingerprintBindsOperationAndFence(t *testing.T) {
	request := FinalizationRequest{
		OperationID: "operation-1",
		Fence: FinalizationFence{
			RunID:             "run-1",
			RunLeaseID:        "lease-1",
			ExpiresAtUnixNano: 100,
		},
	}
	first, err := FinalizationFingerprint("capture", request)
	if err != nil {
		t.Fatal(err)
	}
	request.Fence.ExpiresAtUnixNano++
	second, err := FinalizationFingerprint("capture", request)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("authority expiry did not change finalization fingerprint")
	}
}

func TestFinalizationFingerprintBindsResetTarget(t *testing.T) {
	target, err := EmptyResetTarget("version-1", TreeIdentity{Digest: CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	request := FinalizationRequest{OperationID: "operation-1", Target: target}
	first, err := FinalizationFingerprint(FinalizationResetKind, request)
	if err != nil {
		t.Fatal(err)
	}
	target.BaseVersionID = "version-2"
	request.Target = target
	second, err := FinalizationFingerprint(FinalizationResetKind, request)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Reset target did not change finalization fingerprint")
	}
}

func TestEmptyResetTargetRejectsNonemptyTree(t *testing.T) {
	if _, err := EmptyResetTarget("version-1", TreeIdentity{Digest: CanonicalEmptyTreeDigest, EntryCount: 1}); err == nil {
		t.Fatal("nonempty tree was accepted as an empty Reset target")
	}
}
