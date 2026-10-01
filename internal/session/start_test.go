package session

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"
)

func TestStartReceiptRoundTrip(t *testing.T) {
	sessionID, runID := uuid.NewV7(), uuid.NewV7()
	raw, err := json.Marshal(startReceipt{SessionID: sessionID.String(), BootRunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"actorId":"`+sessionID.String()+`","bootRunId":"`+runID.String()+`"}` {
		t.Fatalf("receipt = %s", raw)
	}
	decoded, err := startedFromReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != (Started{SessionID: sessionID, BootRunID: runID}) {
		t.Fatalf("decoded = %+v", decoded)
	}
	for _, invalid := range []string{`[]`, `{"actorId":"x","bootRunId":"` + runID.String() + `"}`, `{"actorId":"` + sessionID.String() + `"}`} {
		if _, err := startedFromReceipt([]byte(invalid)); !errors.Is(err, ErrStartReceiptInvalid) {
			t.Fatalf("receipt %s error = %v", invalid, err)
		}
	}
}
