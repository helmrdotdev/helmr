package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The metadata and object body can disagree; admission must bound actual writes.
func TestArtifactDownloadRejectsOversizedBodyBeforeExhaustion(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "uncached"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			base := &fakeCAS{objects: map[string][]byte{}}
			object, err := base.Put(t.Context(), "application/test", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			body := &artifactDownloadBody{Reader: strings.NewReader("body" + strings.Repeat("x", 1<<20))}
			store := &artifactDownloadStore{Store: base, body: body}
			root := t.TempDir()
			materializer := WorkspaceMaterializer{CAS: store}
			if cached {
				materializer.ArtifactCacheDir = filepath.Join(root, "cache")
			}
			path, cleanup, err := materializer.restoreCASObject(t.Context(), root, "image", workerapi.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType})
			cleanup()
			if err == nil || path != "" {
				t.Fatalf("oversized body accepted: path=%q err=%v", path, err)
			}
			if body.read != object.SizeBytes+1 || !body.closed {
				t.Fatalf("read=%d closed=%t", body.read, body.closed)
			}
			if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					t.Errorf("failed download retained file %s", path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArtifactCopyBoundsWritesAndRequiresEOF(t *testing.T) {
	for _, test := range []struct {
		name, body string
		wantErr    bool
	}{
		{"exact", "body", false}, {"short", "bod", true}, {"oversized", "body-extra", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := copyCASObject(t.Context(), &output, strings.NewReader(test.body), 4)
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v", err)
			}
			if output.Len() > 4 {
				t.Fatalf("wrote beyond descriptor: %d", output.Len())
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	if _, err := copyCASObject(ctx, &output, strings.NewReader("body"), 4); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("cancelled copy wrote %d bytes, err=%v", output.Len(), err)
	}
}

type artifactDownloadStore struct {
	cas.Store
	body *artifactDownloadBody
}

func (s *artifactDownloadStore) Get(context.Context, string) (io.ReadCloser, error) {
	return s.body, nil
}

type artifactDownloadBody struct {
	*strings.Reader
	read   int64
	closed bool
}

func (r *artifactDownloadBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}
func (r *artifactDownloadBody) Close() error { r.closed = true; return nil }
