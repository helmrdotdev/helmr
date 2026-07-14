package control

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkerEnrollmentGuardBoundsRateAndConcurrency(t *testing.T) {
	guard := newWorkerEnrollmentGuard()
	now := time.Now()
	for range workerChallengePerSourceLimit {
		if !guard.allowChallenge("192.0.2.1", now) {
			t.Fatal("challenge was rejected before the source limit")
		}
	}
	if guard.allowChallenge("192.0.2.1", now) {
		t.Fatal("challenge source limit was not enforced")
	}
	if !guard.allowChallenge("192.0.2.2", now) {
		t.Fatal("one source exhausted another source's allowance")
	}
	for range workerEnrollmentVerificationMax {
		if !guard.beginVerification(context.Background()) {
			t.Fatal("verification was rejected before the concurrency limit")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if guard.beginVerification(canceled) {
		t.Fatal("verification concurrency limit was not enforced")
	}
	for range workerEnrollmentVerificationMax {
		guard.endVerification()
	}
	if !guard.beginVerification(context.Background()) {
		t.Fatal("verification capacity was not released")
	}
	guard.endVerification()
}

func TestWorkerEnrollmentGuardAllowsTargetFleetBurstFromOneSource(t *testing.T) {
	guard := newWorkerEnrollmentGuard()
	now := time.Now()
	for index := range 200 {
		if !guard.allowChallenge("192.0.2.1", now) || !guard.allowEnrollment("192.0.2.1", now) {
			t.Fatalf("target fleet request %d was rate limited", index+1)
		}
	}
}

func TestWorkerEnrollmentSourceUsesLastForwardedAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/worker/enrollment", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.8")
	if got := workerEnrollmentSource(request); got != "203.0.113.8" {
		t.Fatalf("source = %q", got)
	}
}
