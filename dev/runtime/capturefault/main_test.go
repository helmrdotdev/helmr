package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/helmrdotdev/helmr/internal/cas"
	casS3 "github.com/helmrdotdev/helmr/internal/cas/s3"
)

func TestDedicatedBucketRootOperations(t *testing.T) {
	handler := bucketHandler("bucket", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for path, want := range map[string]int{"/bucket?versions": 204, "/bucket?uploads": 204, "/bucket/key": 204, "/bucket-other/key": 403, "/other": 403} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d want %d", path, w.Code, want)
		}
	}
}

func TestQuarantineIsSignedOnceForUpstream(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-original")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-original-secret")
	t.Setenv("AWS_SESSION_TOKEN", "test-original-token")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-original/") {
			t.Error("missing original signing identity")
		}
		r.URL.Scheme = "https"
		r.URL.Host = "s3.us-east-1.amazonaws.com"
		r.Host = r.URL.Host
		transport := signedTransport{credentials.NewStaticCredentialsProvider("test-upstream", "test-upstream-secret", "test-upstream-token"), "us-east-1"}
		if err := transport.sign(r); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-upstream/") || r.Header.Get("X-Amz-Security-Token") != "test-upstream-token" || r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256([]byte("x"))) {
			t.Error("missing upstream signing identity/hash")
		}
		if r.Header.Get("X-Amz-Checksum-Sha256") == "" || r.Header.Get("If-None-Match") != "*" {
			t.Error("upload checks lost")
		}
		if body, err := io.ReadAll(r.Body); err != nil || string(body) != "x" {
			t.Errorf("body=%q err=%v", body, err)
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	store, err := casS3.New(t.Context(), "s3://bucket?endpoint="+url.QueryEscape(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x"))), SizeBytes: 1, MediaType: "application/octet-stream"}
	if err := store.StreamQuarantine(t.Context(), "verification", descriptor, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyOneArmedMemoryUploadFails(t *testing.T) {
	forwarded := 0
	f := &fault{delay: time.Millisecond, forward: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded++; w.WriteHeader(204) })}
	request := func(method, path, media string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Content-Type", media)
		w := httptest.NewRecorder()
		f.ServeHTTP(w, r)
		return w.Code
	}
	if request("PUT", "/bucket/key", cas.CheckpointMemoryMediaType) != 204 {
		t.Fatal("unarmed request failed")
	}
	if request("POST", "/__fault", "") != 200 {
		t.Fatal("arm")
	}
	if request("PUT", "/bucket/disk", "application/octet-stream") != 204 {
		t.Fatal("other object failed")
	}
	if request("POST", "/bucket/memory?uploads=", cas.CheckpointMemoryMediaType) != 403 {
		t.Fatal("memory upload did not fail")
	}
	if f.started.IsZero() || f.failed.Sub(f.started) < f.delay {
		t.Fatal("hold not observed")
	}
	if request("PUT", "/bucket/next", cas.CheckpointMemoryMediaType) != 204 || forwarded != 3 {
		t.Fatal("later capture affected")
	}
	if request("POST", "/__fault", "") != 409 {
		t.Fatal("rearming permitted")
	}
}
