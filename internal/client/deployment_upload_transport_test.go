package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestDeploymentObjectUploadRefusesEveryRedirect(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirected.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer destination.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "close")
				http.Redirect(w, r, destination.URL, status)
			}))
			defer origin.Close()
			client, err := New(origin.URL, WithBearerToken("upload-secret"))
			if err != nil {
				t.Fatal(err)
			}
			err = client.UploadDeploymentBundleObject(t.Context(), testUploadBundleDigest, EnvironmentScopeOptions{}, deploymentUploadTestPlan(6), writeDeploymentUploadTestObject(t, "object"), nil)
			if err == nil || redirected.Load() != 0 {
				t.Fatalf("redirect requests=%d error=%v", redirected.Load(), err)
			}
		})
	}
}

func TestDeploymentObjectUploadWaitsForCapacityWithoutReplanning(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Expect") != "100-continue" {
			t.Error("upload must allow early admission rejection")
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "object" {
			t.Errorf("body=%q error=%v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = client.UploadDeploymentBundleObject(t.Context(), testUploadBundleDigest, EnvironmentScopeOptions{}, deploymentUploadTestPlan(6), writeDeploymentUploadTestObject(t, "object"), nil)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestDeploymentObjectUploadCancelsCapacityWait(t *testing.T) {
	var calls atomic.Int32
	client := newDeploymentUploadTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"7200"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := client.UploadDeploymentBundleObject(ctx, testUploadBundleDigest, EnvironmentScopeOptions{}, deploymentUploadTestPlan(6), writeDeploymentUploadTestObject(t, "object"), nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestDeploymentObjectUploadLifetimeIncludesCapacityWait(t *testing.T) {
	object := writeDeploymentUploadTestObject(t, "object")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c, err := New("https://example.test", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"9000"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
			}
			time.Sleep(time.Minute)
			return deploymentUploadTestResponse(r), nil
		})}))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err := c.UploadDeploymentBundleObject(t.Context(), testUploadBundleDigest, EnvironmentScopeOptions{}, deploymentUploadTestPlan(6), object, nil); err != nil {
			t.Fatal(err)
		}
		if calls != 2 || time.Since(started) != 151*time.Minute {
			t.Fatalf("calls=%d elapsed=%s", calls, time.Since(started))
		}
	})
}

func TestDeploymentObjectUploadDeadlineAndCallerCancellation(t *testing.T) {
	object := writeDeploymentUploadTestObject(t, "object")
	for _, callerDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(callerDeadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, err := New("https://example.test", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"9223372036854775807"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
				})}))
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if callerDeadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
				}
				started := time.Now()
				err = c.UploadDeploymentBundleObject(ctx, testUploadBundleDigest, EnvironmentScopeOptions{}, deploymentUploadTestPlan(6), object, nil)
				want := 3 * time.Hour
				if callerDeadline {
					want = 5 * time.Second
				}
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != want {
					t.Fatalf("elapsed=%s error=%v", time.Since(started), err)
				}
				if !callerDeadline && !strings.Contains(err.Error(), "client object-upload deadline 3h0m0s exceeded") {
					t.Fatalf("deadline owner missing: %v", err)
				}
			})
		})
	}
}
