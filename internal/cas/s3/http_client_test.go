package s3

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type storeTestHTTPClient func(*http.Request) (*http.Response, error)

func (f storeTestHTTPClient) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3ConfiguredTransportOwnsReadsAndUploads(t *testing.T) {
	// Use fixture credentials and empty config files; no host authentication or
	// external endpoint is needed to exercise the actual SDK request pipeline.
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent-credentials"))
	data := []byte("immutable optional transfer")
	d := cas.Descriptor{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data)), MediaType: "application/octet-stream"}
	var heads, uploads int
	client := storeTestHTTPClient(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodHead:
			heads++
		case http.MethodPut:
			uploads++
			var reader io.Reader = request.Body
			if strings.Contains(request.Header.Get("Content-Encoding"), "aws-chunked") {
				reader = httputil.NewChunkedReader(reader)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(body, data) {
				t.Fatalf("SDK upload differs: %q", body)
			}
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {strconv.Itoa(len(data))}, "Content-Type": {d.MediaType}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	})
	store, err := New(t.Context(), "s3://fixture/objects?endpoint=https://storage.invalid", WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(t.Context(), d.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), d.MediaType, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if heads != 1 || uploads != 1 {
		t.Fatalf("SDK bypassed configured transport: heads=%d uploads=%d", heads, uploads)
	}
}
