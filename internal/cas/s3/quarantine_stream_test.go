package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// This reader cannot seek. It also refuses to supply the complete object until
// the server sees request headers, detecting signer/checksum pre-buffering.
type streamingQuarantineReader struct {
	reader  *bytes.Reader
	started <-chan struct{}
	ctx     context.Context
	read    int
}

func (r *streamingQuarantineReader) Read(p []byte) (int, error) {
	if r.read >= 64<<10 {
		select {
		case <-r.started:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestS3StreamQuarantineUsesNonSeekableBody(t *testing.T) {
	for _, tls := range []bool{true, false} {
		t.Run(map[bool]string{true: "https", false: "http"}[tls], func(t *testing.T) {
			payload := bytes.Repeat([]byte("streaming object\n"), 128<<10)
			expected := cas.Descriptor{Digest: sha256sum.DigestBytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/octet-stream"}
			started := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				if r.Method != http.MethodPut || r.URL.Path != "/bucket/cas/quarantine/owner/sha256/"+expected.Digest[len("sha256:"):] {
					t.Errorf("unexpected upload target: %s %s", r.Method, r.URL.Path)
				}
				checksum := sha256.Sum256(payload)
				for name, want := range map[string]string{
					"Content-Type": expected.MediaType, "If-None-Match": "*",
					"X-Amz-Content-Sha256":  strings.TrimPrefix(expected.Digest, "sha256:"),
					"X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(checksum[:]),
					"X-Amz-Tagging":         "helmr-expirable=true",
				} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				if r.ContentLength != expected.SizeBytes {
					t.Errorf("length = %d", r.ContentLength)
				}
				actual, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(actual, payload) {
					t.Errorf("body mismatch: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			}))
			if tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			store := testStreamingQuarantineStore(server)
			if err := store.StreamQuarantine(ctx, "owner", expected, &streamingQuarantineReader{reader: bytes.NewReader(payload), started: started, ctx: ctx}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestS3StreamQuarantineDoesNotReplayIncomingBody(t *testing.T) {
	for _, code := range []string{"InternalError", "BadDigest", "XAmzContentSHA256Mismatch"} {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "<Error><Code>"+code+"</Code><Message>rejected</Message></Error>")
			}))
			defer server.Close()
			payload := []byte("object")
			expected := cas.Descriptor{Digest: sha256sum.DigestBytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/octet-stream"}
			err := testStreamingQuarantineStore(server).StreamQuarantine(t.Context(), "owner", expected, struct{ io.Reader }{bytes.NewReader(payload)})
			if err == nil || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
			if code != "InternalError" && !errors.Is(err, cas.ErrDigestMismatch) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestS3StreamQuarantineConditionalWrite(t *testing.T) {
	payload := []byte("object")
	expected := cas.Descriptor{Digest: sha256sum.DigestBytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/octet-stream"}
	checksum, _ := descriptorChecksum(expected.Digest)
	for _, mode := range []string{"existing", "conflicting descriptor", "concurrent conflict"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeS3Client{putObjectErr: &smithy.GenericAPIError{Code: "PreconditionFailed"}, headObject: &awss3.HeadObjectOutput{ChecksumSHA256: aws.String(checksum), ContentLength: aws.Int64(expected.SizeBytes), ContentType: aws.String(expected.MediaType)}}
			if mode == "conflicting descriptor" {
				client.headObject.ContentType = aws.String("text/plain")
			}
			if mode == "concurrent conflict" {
				client.putObjectErr = &smithy.GenericAPIError{Code: "ConditionalRequestConflict"}
			}
			store := &Store{client: client, bucket: "bucket", prefix: "cas"}
			err := store.StreamQuarantine(t.Context(), "owner", expected, struct{ io.Reader }{bytes.NewReader(payload)})
			switch mode {
			case "existing":
				if err != nil || client.headObjectCalls != 1 {
					t.Fatalf("heads=%d error=%v", client.headObjectCalls, err)
				}
			case "conflicting descriptor":
				if !errors.Is(err, cas.ErrDigestMismatch) {
					t.Fatalf("error=%v", err)
				}
			case "concurrent conflict":
				if !errors.Is(err, cas.ErrUploadConflict) || client.headObjectCalls != 0 {
					t.Fatalf("heads=%d error=%v", client.headObjectCalls, err)
				}
			}
			if client.putObjectCalls != 1 || aws.ToString(client.putObject.IfNoneMatch) != "*" {
				t.Fatal("conditional upload was retried or allowed overwrite")
			}
		})
	}
}

func testStreamingQuarantineStore(server *httptest.Server) *Store {
	client := awss3.New(awss3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		HTTPClient: server.Client(), RetryMaxAttempts: 3,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
	})
	return &Store{client: client, bucket: "bucket", prefix: "cas"}
}
