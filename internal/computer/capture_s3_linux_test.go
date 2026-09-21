//go:build linux

package computer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
)

func TestCandidateUsesDataStoreWithoutSecondStage(t *testing.T) {
	// Exercise the actual S3 publisher against a local HTTP fixture. No cloud
	// credentials or external resources are used.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	dir := t.TempDir()
	var mu sync.Mutex
	var object []byte
	var media, objectPath string
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			puts++
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Tagging") != "" {
				t.Error("disk upload must be create-only and not expiry-tagged")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if object != nil {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256(body)
			if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
				t.Error("ciphertext checksum mismatch")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			object, media, objectPath = body, r.Header.Get("Content-Type"), r.URL.Path
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if r.URL.Path != objectPath || object == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(object)))
			w.Header().Set("Content-Type", media)
		case http.MethodGet:
			if r.URL.Path != objectPath || object == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", media)
			_, _ = w.Write(object)
		default:
			t.Errorf("unexpected request: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	objects, err := cass3.New(t.Context(), "s3://data/computers?endpoint="+url.QueryEscape(server.URL), cass3.WithTempDir(filepath.Join(dir, "must-not-create-another-stage")))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: objects, Cipher: cipher}
	source := filepath.Join(dir, "source")
	plain := bytes.Repeat([]byte{3}, 4096)
	if err := os.WriteFile(source, plain, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.Capture(t.Context(), diskTestComputer, source, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	for range 2 {
		if err := candidate.Upload(t.Context(), objects); err != nil {
			t.Fatal(err)
		}
	}
	artifact := candidate.Artifact()
	mu.Lock()
	gotPath, gotPuts := objectPath, puts
	mu.Unlock()
	if gotPath != "/data/computers/sha256/"+artifact.Object.Digest[len("sha256:"):] || gotPuts != 2 {
		t.Fatalf("unexpected upload namespace/retry: %q, %d", gotPath, gotPuts)
	}
	restored := filepath.Join(dir, "restored")
	if err := store.Restore(t.Context(), diskTestComputer, artifact, restored, int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("S3 restore differs: %v", err)
	}
}
