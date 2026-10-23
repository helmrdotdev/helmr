package controlplane

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkerEnrollmentGuardBoundsEnrollmentRate(t *testing.T) {
	guard := newWorkerEnrollmentGuard()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for range workerEnrollmentPerSourceLimit {
		if !guard.allowEnrollment("192.0.2.1", now) {
			t.Fatal("enrollment was rejected before the source limit")
		}
	}
	if guard.allowEnrollment("192.0.2.1", now) {
		t.Fatal("enrollment source limit was not enforced")
	}
	for range workerEnrollmentGlobalLimit - workerEnrollmentPerSourceLimit {
		if !guard.allowEnrollment("192.0.2.2", now) {
			t.Fatal("enrollment was rejected before the global limit")
		}
	}
	if guard.allowEnrollment("192.0.2.3", now) {
		t.Fatal("global limit did not reject an unused source")
	}
	if guard.allowEnrollment("192.0.2.3", now.Add(workerEnrollmentRateWindow-time.Nanosecond)) {
		t.Fatal("global allowance reset before the window elapsed")
	}
	for _, source := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		if !guard.allowEnrollment(source, now.Add(workerEnrollmentRateWindow)) {
			t.Fatalf("enrollment for %s rejected at the exact window boundary", source)
		}
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
	request := httptest.NewRequest("POST", "/worker/v1/enrollment", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.8")
	if got := workerEnrollmentSource(request); got != "203.0.113.8" {
		t.Fatalf("source = %q", got)
	}
}
