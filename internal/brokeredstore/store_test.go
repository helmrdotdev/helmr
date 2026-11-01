package brokeredstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// grantServer is a Control Plane grant client and the signed storage it grants.
type grantServer struct {
	mu        sync.Mutex
	storage   *httptest.Server
	objects   map[string][]byte
	reads     []workerapi.StorageReadRequest
	writes    int
	completed []cas.CompletedPart
	aborted   bool
	expired   bool // the next storage request is refused as an expired grant
	failParts int  // part uploads to refuse with a server error before accepting one
	dropParts int  // part uploads to fail at the connection before accepting one
	partGrant int  // part grants issued
	partSize  int64
}

func newGrantServer(t *testing.T) *grantServer {
	g := &grantServer{objects: map[string][]byte{}, partSize: 4}
	g.storage = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.storage.Close)
	return g
}

func (g *grantServer) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.expired {
		g.expired = false
		w.WriteHeader(http.StatusForbidden)
		return
	}
	digest := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		body, ok := g.objects[digest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int64
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[start : end+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if part := r.URL.Query().Get("part"); part != "" {
			if g.dropParts > 0 {
				g.dropParts--
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			if g.failParts > 0 {
				g.failParts--
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", `"etag-`+part+`"`)
			g.objects[digest+"#"+part] = body
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, exists := g.objects[digest]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		g.objects[digest] = body
	}
}

func (g *grantServer) signed(method, digest, query string) cas.SignedRequest {
	return cas.SignedRequest{Method: method, URL: g.storage.URL + "/" + digest + query, ExpiresAt: time.Now().Add(time.Minute)}
}

func (g *grantServer) StorageRead(_ context.Context, r workerapi.StorageReadRequest) (workerapi.StorageReadGrant, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, r)
	return workerapi.StorageReadGrant{Digest: r.Digest, Get: g.signed(http.MethodGet, r.Digest, ""), Head: g.signed(http.MethodHead, r.Digest, "")}, nil
}

func (g *grantServer) StorageWrite(_ context.Context, r workerapi.StorageWriteRequest) (workerapi.StorageWriteResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes++
	if _, ok := g.objects[r.Object.Digest]; ok {
		return workerapi.StorageWriteResponse{Exists: true}, nil
	}
	if r.Object.SizeBytes > g.partSize {
		parts := int32((r.Object.SizeBytes + g.partSize - 1) / g.partSize)
		return workerapi.StorageWriteResponse{Upload: &cas.Upload{ID: "upload", PartSize: g.partSize, Parts: parts}}, nil
	}
	put := g.signed(http.MethodPut, r.Object.Digest, "")
	put.Header = http.Header{"If-None-Match": {"*"}}
	return workerapi.StorageWriteResponse{Put: &put}, nil
}

func (g *grantServer) StorageParts(_ context.Context, r workerapi.StoragePartsRequest) (workerapi.StoragePartsResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.partGrant += len(r.Parts)
	var out workerapi.StoragePartsResponse
	for _, n := range r.Parts {
		out.Parts = append(out.Parts, g.signed(http.MethodPut, r.Object.Digest, fmt.Sprintf("?part=%d", n)))
	}
	return out, nil
}

func (g *grantServer) StorageComplete(_ context.Context, r workerapi.StorageCompleteRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.completed = r.Parts
	var body []byte
	for _, p := range r.Parts {
		body = append(body, g.objects[fmt.Sprintf("%s#%d", r.Object.Digest, p.Number)]...)
	}
	g.objects[r.Object.Digest] = body
	return nil
}

func (g *grantServer) StorageAbort(context.Context, workerapi.StorageAbortRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.aborted = true
	return nil
}

func scoped(t *testing.T) context.Context {
	return WithScope(t.Context(), workerapi.StorageScope{Computer: &workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: "env", OwnerID: "computer", Epoch: 1, AllocationID: "allocation"}})
}

// A request carries the authority of its context or fails before any grant.
func TestStorageRequiresAuthority(t *testing.T) {
	g := newGrantServer(t)
	store, err := New(g, workerapi.StorageCAS, g.storage.Client())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256sum.DigestBytes([]byte("x"))
	if _, err := store.Get(t.Context(), digest); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("get without authority = %v", err)
	}
	if _, err := store.GetRange(t.Context(), digest, 4, 0, 1); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("range without authority = %v", err)
	}
	if len(g.reads) != 0 {
		t.Fatalf("grants requested without authority: %+v", g.reads)
	}
	// A bound range source keeps the authority of the context that created it.
	body := []byte("abcd")
	g.objects[sha256sum.DigestBytes(body)] = body
	bound := BindRanges(scoped(t), store)
	reader, err := bound.GetRange(context.Background(), sha256sum.DigestBytes(body), 4, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(reader)
	reader.Close()
	if string(got) != "bc" || g.reads[0].Scope.Computer == nil || g.reads[0].Scope.Computer.AllocationID != "allocation" {
		t.Fatalf("bound range = %q, %+v", got, g.reads)
	}
	platform, _ := New(g, workerapi.StoragePlatform, g.storage.Client())
	if _, err := platform.Publish(scoped(t), cas.Descriptor{Digest: digest, SizeBytes: 1, MediaType: "x"}, nil); err == nil {
		t.Fatal("platform store accepted a write")
	}
}

// Reads verify the digest, reuse a live grant, and renew a grant storage refuses.
func TestStorageReadsVerifyAndRenew(t *testing.T) {
	g := newGrantServer(t)
	store, _ := New(g, workerapi.StorageCAS, g.storage.Client())
	ctx := scoped(t)
	body := []byte("object bytes")
	digest := sha256sum.DigestBytes(body)
	g.objects[digest] = body
	object, err := store.Stat(ctx, digest)
	if err != nil || object.SizeBytes != int64(len(body)) {
		t.Fatalf("stat = %+v, %v", object, err)
	}
	reader, err := store.Get(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get = %q, %v", got, err)
	}
	reader.Close()
	if len(g.reads) != 1 {
		t.Fatalf("live grant was not reused: %d requests", len(g.reads))
	}
	g.expired = true
	if _, err := store.Stat(ctx, digest); err != nil || len(g.reads) != 2 {
		t.Fatalf("refused grant not renewed: %v, %d requests", err, len(g.reads))
	}
	g.objects[digest] = []byte("poisoned byte")
	reader, _ = store.Get(ctx, digest)
	if _, err := io.ReadAll(reader); !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("poisoned bytes = %v", err)
	}
	if _, err := store.Stat(ctx, sha256sum.DigestBytes([]byte("missing"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object = %v", err)
	}
}

func publishedFile(t *testing.T, body []byte) (*os.File, cas.Descriptor) {
	t.Helper()
	path := t.TempDir() + "/object"
	if err := os.WriteFile(path, body, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file, cas.Descriptor{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body)), MediaType: "application/octet-stream"}
}
