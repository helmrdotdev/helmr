package controlplane

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
)

func TestValidateDeploymentSourceArtifactArchiveRequiresCanonicalSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "package.json"),
		[]byte(`{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"name":"test","packageManager":"bun@1.3.10","type":"module"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "bun.lock"),
		[]byte(`{"configVersion":1,"lockfileVersion":1,"packages":{},"workspaces":{"":{"name":"test"}}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "helmr.config.ts"),
		[]byte(`export default { dirs: ["tasks"] }`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	canonical, cleanup, err := archive.CreateTarWithOptions(
		root,
		t.TempDir(),
		archive.TarOptions{CanonicalSource: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := inspectDeploymentSourceArtifactArchive(canonical.Path); err != nil {
		t.Fatalf("canonical source rejected: %v", err)
	}

	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	payload := []byte(`{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"name":"test","packageManager":"bun@1.3.10","type":"module"}`)
	if err := writer.WriteHeader(&tar.Header{
		Name:    "package.json",
		Mode:    0o644,
		Size:    int64(len(payload)),
		ModTime: time.Unix(1, 0),
		Format:  tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	noncanonical := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(noncanonical, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectDeploymentSourceArtifactArchive(noncanonical); err == nil ||
		!strings.Contains(err.Error(), "noncanonical") {
		t.Fatalf("noncanonical source error = %v", err)
	}
}

func TestLimitAPIRequestBodyAllowsCanonicalSourceEnvelopeOnlyForDeployments(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		size       int64
		wantStatus int
	}{
		{
			name:       "scoped deployment",
			path:       "/api/projects/project/environments/env/deployments",
			size:       archive.MaxSourceArtifactBytes + 1,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "environment deployment",
			path:       "/api/deployments",
			size:       archive.MaxSourceArtifactBytes + 1,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "other API route",
			path:       "/api/runs",
			size:       archive.MaxSourceArtifactBytes + 1,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:       "deployment envelope exceeded",
			path:       "/api/deployments",
			size:       deploymentRequestBodyLimit + 1,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
			request.ContentLength = test.size
			response := httptest.NewRecorder()
			limitAPIRequestBody(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
