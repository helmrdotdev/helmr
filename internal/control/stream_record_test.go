package control

import "testing"

func TestStreamRecordFingerprintIncludesCorrelationAndContentType(t *testing.T) {
	base, err := streamRecordFingerprint([]byte(`{"approved":true}`), "thread-1", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	changedCorrelation, err := streamRecordFingerprint([]byte(`{"approved":true}`), "thread-2", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	changedContentType, err := streamRecordFingerprint([]byte(`{"approved":true}`), "thread-1", "application/vnd.helmr+json")
	if err != nil {
		t.Fatal(err)
	}
	sameCanonicalData, err := streamRecordFingerprint([]byte(`{"approved": true}`), "thread-1", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if base == changedCorrelation {
		t.Fatal("fingerprint did not change when correlation_id changed")
	}
	if base == changedContentType {
		t.Fatal("fingerprint did not change when content_type changed")
	}
	if base != sameCanonicalData {
		t.Fatal("fingerprint changed for equivalent canonical JSON data")
	}
}
