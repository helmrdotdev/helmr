package deployment

import "testing"

func TestParseRetryManifestAcceptsOnlyCompleteNormalizedShape(t *testing.T) {
	valid := []string{
		`{"enabled":false}`,
		`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"factor":2,"jitter":"full"}}`,
	}
	for _, raw := range valid {
		if _, err := ParseRetryManifest([]byte(raw)); err != nil {
			t.Fatalf("ParseRetryManifest(%s): %v", raw, err)
		}
	}

	invalid := []string{
		`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"factor":2}}`,
		`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"factor":2.5,"jitter":"full"}}`,
		`{"enabled":false,"maxAttempts":1}`,
		`{"enabled":false,"enabled":true}`,
		`{"enabled":false,"unknown":true}`,
	}
	for _, raw := range invalid {
		if _, err := ParseRetryManifest([]byte(raw)); err == nil {
			t.Fatalf("ParseRetryManifest(%s) accepted an invalid manifest", raw)
		}
	}
}
