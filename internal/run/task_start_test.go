package run

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"
)

func TestTaskStartReceiptRoundTrip(t *testing.T) {
	runID := uuid.NewV7()
	raw, err := json.Marshal(taskStartReceipt{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskStartedFromReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RunID != runID || decoded.Replayed {
		t.Fatalf("decoded = %+v", decoded)
	}
	for _, raw := range []string{`[]`, `{"run_id":"00000000-0000-0000-0000-000000000000"}`, `{"run_id":"run"}`} {
		if _, err := taskStartedFromReceipt([]byte(raw)); !errors.Is(err, ErrTaskStartReceiptInvalid) {
			t.Fatalf("receipt %s error = %v", raw, err)
		}
	}
}
