package controlplane

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkerEnrollmentGuardBoundsEnrollmentRate(t *testing.T) {
	guard := newWorkerEnrollmentGuard()
	now := time.Now()
	for range workerEnrollmentPerSourceLimit {
		if !guard.allowEnrollment("192.0.2.1", now) {
			t.Fatal("enrollment was rejected before the source limit")
		}
	}
	if guard.allowEnrollment("192.0.2.1", now) {
		t.Fatal("enrollment source limit was not enforced")
	}
	if !guard.allowEnrollment("192.0.2.2", now) {
		t.Fatal("one source exhausted another source's allowance")
	}
}

func TestWorkerEnrollmentGuardAllowsTargetFleetBurstFromOneSource(t *testing.T) {
	guard := newWorkerEnrollmentGuard()
	now := time.Now()
	for index := range 200 {
		if !guard.allowEnrollment("192.0.2.1", now) {
			t.Fatalf("target worker-group request %d was rate limited", index+1)
		}
	}
}

func TestWorkerEnrollmentSourceUsesLastForwardedAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/worker/v0/enrollment", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.8")
	if got := workerEnrollmentSource(request); got != "203.0.113.8" {
		t.Fatalf("source = %q", got)
	}
}
