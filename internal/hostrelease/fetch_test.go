package hostrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// fakeSource serves objects by digest from an HTTP server, as the Control
// Plane's read grants do.
type fakeSource struct {
	release *workerapi.Release
	server  *httptest.Server
}

func (s fakeSource) Release(context.Context) (*workerapi.Release, error) { return s.release, nil }

func (s fakeSource) ReleaseRead(_ context.Context, digest string) (workerapi.StorageReadGrant, error) {
	return workerapi.StorageReadGrant{Digest: digest, Get: cas.SignedRequest{Method: http.MethodGet, URL: s.server.URL + "/" + digest}}, nil
}

func object(data []byte) workerapi.ReleaseObject {
	sum := sha256.Sum256(data)
	return workerapi.ReleaseObject{Digest: "sha256:" + hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
}

func serve(t *testing.T, objects map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := objects[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// The signed index verifies, and an asset whose bytes are not the ones the
// index lists is refused before it is kept.
func TestFetchVerifiesTheIndexAndEveryAsset(t *testing.T) {
	index, err := os.ReadFile("../release/testdata/v0.2.9-release-index.json")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile("../release/testdata/v0.2.9-release-index.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	indexObject, signatureObject := object(index), object(signature)
	objects := map[string][]byte{indexObject.Digest: index, signatureObject.Digest: signature}
	server := serve(t, objects)
	source := fakeSource{release: &workerapi.Release{Version: "v0.2.9", Index: indexObject, Signature: signatureObject}, server: server}
	dir := t.TempDir()
	_, err = Fetch(t.Context(), source, server.Client(), dir)
	if err == nil || !strings.Contains(err.Error(), "worker-host-artifacts.tar") {
		t.Fatalf("fetch with a missing asset: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("an unverified asset was kept: %v", entries)
	}

	// An index the release workflow did not sign is refused outright.
	tampered := []byte(strings.Replace(string(index), `"v0.2.9"`, `"v0.2.9" `, 1))
	tamperedObject := object(tampered)
	objects[tamperedObject.Digest] = tampered
	source.release.Index = tamperedObject
	if _, err := Fetch(t.Context(), source, server.Client(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("fetch with a tampered index: %v", err)
	}
}

func TestFetchNeedsARelease(t *testing.T) {
	if _, err := Fetch(t.Context(), fakeSource{}, http.DefaultClient, t.TempDir()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("fetch without a release: %v", err)
	}
}
