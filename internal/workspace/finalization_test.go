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
