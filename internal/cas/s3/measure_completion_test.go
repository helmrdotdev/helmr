package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"golang.org/x/sync/errgroup"
)

// TestMeasureMultipartCompletion times CompleteUpload of one object of
// HELMR_MEASURE_COMPLETION_BYTES (the largest memory checkpoint a deployment
// admits) in a real bucket, uploading its parts through signed requests as a
// worker does. It runs with Control Plane storage credentials against
// HELMR_MEASURE_COMPLETION_URI and fails when completion reaches
// HELMR_MEASURE_COMPLETION_LIMIT_SECONDS (the load balancer idle timeout,
// 180 by default). The measured object is deleted afterwards.
func TestMeasureMultipartCompletion(t *testing.T) {
	uri := os.Getenv("HELMR_MEASURE_COMPLETION_URI")
	if uri == "" {
		t.Skip("HELMR_MEASURE_COMPLETION_URI is not set")
	}
	size, err := strconv.ParseInt(os.Getenv("HELMR_MEASURE_COMPLETION_BYTES"), 10, 64)
	if err != nil || size < MultipartThresholdBytes {
		t.Fatalf("HELMR_MEASURE_COMPLETION_BYTES must be at least %d", MultipartThresholdBytes)
	}
	limit := 180 * time.Second
	if raw := os.Getenv("HELMR_MEASURE_COMPLETION_LIMIT_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			t.Fatal("HELMR_MEASURE_COMPLETION_LIMIT_SECONDS must be a positive integer")
		}
		limit = time.Duration(seconds) * time.Second
	}
	store, err := New(t.Context(), uri)
	if err != nil {
		t.Fatal(err)
	}
	seed := uint64(time.Now().UnixNano())
	// Incompressible bytes, regenerated per part instead of held in memory.
	bytesAt := func(offset, length int64) []byte {
		out := make([]byte, length)
		source := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24), byte(offset >> 20), byte(offset >> 28), byte(offset >> 36)})
		_, _ = source.Read(out)
		return out
	}
	upload, _ := store.UploadLayout(cas.Descriptor{Digest: "sha256:" + hex.EncodeToString(make([]byte, sha256.Size)), SizeBytes: size, MediaType: cas.CheckpointMemoryMediaType})
	hash := sha256.New()
	for offset := int64(0); offset < size; offset += upload.PartSize {
		_, _ = hash.Write(bytesAt(offset, min(upload.PartSize, size-offset)))
	}
	descriptor := cas.Descriptor{Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), SizeBytes: size, MediaType: cas.CheckpointMemoryMediaType}
	upload, err = store.CreateUpload(t.Context(), "measure-"+strconv.FormatUint(seed, 16), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]cas.CompletedPart, upload.Parts)
	started := time.Now()
	group, ctx := errgroup.WithContext(t.Context())
	group.SetLimit(4)
	for number := int32(1); number <= upload.Parts; number++ {
		group.Go(func() error {
			offset := int64(number-1) * upload.PartSize
			length := min(upload.PartSize, size-offset)
			grant, err := store.SignPart(ctx, descriptor, upload, number, time.Now().Add(5*time.Minute))
			if err != nil {
				return err
			}
			request, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, bytes.NewReader(bytesAt(offset, length)))
			if err != nil {
				return err
			}
			request.Header = grant.Header.Clone()
			request.ContentLength = length
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || response.Header.Get("ETag") == "" {
				return fmt.Errorf("part %d: %s", number, response.Status)
			}
			parts[number-1] = cas.CompletedPart{Number: number, ETag: response.Header.Get("ETag")}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal(errorsJoinAbort(t, store, upload, descriptor, err))
	}
	uploaded := time.Since(started)
	started = time.Now()
	object, err := store.CompleteUpload(t.Context(), descriptor, upload, parts)
	completed := time.Since(started)
	t.Cleanup(func() { _ = store.Delete(t.Context(), descriptor.Digest) })
	if err != nil {
		t.Fatal(err)
	}
	path := "server-side checksum"
	if size > s3CopyObjectMaxBytes {
		path = "read back through the Control Plane"
	}
	t.Logf("object %s, %d bytes in %d parts (%s): parts uploaded in %s, completion took %s of a %s limit", object.Key, size, upload.Parts, path, uploaded.Round(time.Millisecond), completed.Round(time.Millisecond), limit)
	if completed >= limit {
		t.Fatalf("completion took %s, at or beyond the %s limit", completed, limit)
	}
}

func errorsJoinAbort(t *testing.T, store *Store, upload cas.Upload, descriptor cas.Descriptor, err error) error {
	if abortErr := store.AbortUpload(t.Context(), upload, descriptor.Digest); abortErr != nil {
		return fmt.Errorf("%w; abort: %w", err, abortErr)
	}
	return err
}
