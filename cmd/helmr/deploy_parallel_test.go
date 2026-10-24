package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func parallelUploadFixture(t testing.TB, count int) (bundle.Directory, []string) {
	t.Helper()
	directory := bundle.Directory{Digest: "sha256:" + strings.Repeat("a", 64), BundleJSON: []byte(`{}`), Objects: map[string]string{}}
	digests := make([]string, count)
	root := t.TempDir()
	for i := range count {
		body := []byte(fmt.Sprintf("file-%02d", i))
		digests[i] = sha256sum.DigestBytes(body)
		path := filepath.Join(root, fmt.Sprint(i))
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		directory.Objects[digests[i]] = path
	}
	return directory, digests
}

func fixtureUpload(url, digest string) api.DeploymentBundleUpload {
	upload := deployTestUpload(url, digest)
	upload.URL = url + "/upload/" + digest
	return upload
}

func TestDeploymentUploadsOverlapWithinBoundAndSerializeReports(t *testing.T) {
	directory, digests := parallelUploadFixture(t, 5)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	var active, peak atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/deployment-bundles/upload-plan" {
			uploads := make([]api.DeploymentBundleUpload, 0, 5)
			for _, digest := range digests {
				uploads = append(uploads, fixtureUpload(server.URL, digest))
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{BundleDigest: directory.Digest, Uploads: uploads})
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	controlPlane, err := client.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	done := make(chan error, 1)
	go func() {
		done <- uploadDeploymentBundleObjects(ctx, controlPlane, directory, client.EnvironmentScopeOptions{}, false, newDeployReporter(command, true))
	}()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("two uploads did not overlap")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak uploads=%d", peak.Load())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 10 {
		t.Fatalf("expected start/completion for each object, got %d", len(lines))
	}
	for _, raw := range lines {
		var line cliDeployLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("interleaved output: %v", err)
		}
		if line.Index < 1 || line.Index > 5 || line.Count != 5 || line.Digest != digests[line.Index-1] {
			t.Fatalf("invalid progress identity: %+v", line)
		}
	}
}

func TestParallelDeploymentUploadsReconcileAfterBatchJoins(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			directory, digests := parallelUploadFixture(t, 3)
			var plans, uploads, active atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/deployment-bundles/upload-plan" {
					if active.Load() != 0 {
						t.Error("reconciled before batch joined")
					}
					wanted := digests
					if plans.Add(1) > 1 {
						wanted = digests[2:]
						if missing {
							wanted = digests[1:]
						}
					}
					response := api.DeploymentBundleUploadPlanResponse{BundleDigest: directory.Digest}
					for _, digest := range wanted {
						response.Uploads = append(response.Uploads, fixtureUpload(server.URL, digest))
					}
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				active.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				uploads.Add(1)
				active.Add(-1)
				if strings.HasSuffix(r.URL.Path, digests[1]) {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			controlPlane, err := client.New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			command := newRootCommand()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			err = uploadDeploymentBundleObjects(t.Context(), controlPlane, directory, client.EnvironmentScopeOptions{}, false, newDeployReporter(command, false))
			if (err != nil) != missing {
				t.Fatalf("error=%v", err)
			}
			wantUploads := int32(3)
			if missing {
				wantUploads = 2
			}
			if plans.Load() != 2 || uploads.Load() != wantUploads {
				t.Fatalf("plans=%d uploads=%d", plans.Load(), uploads.Load())
			}
		})
	}
}

type failedUploadReporter struct {
	deployReporter
	err error
}

func (r failedUploadReporter) DeploymentObjectUploadStarted(string, int, int) error { return r.err }

func TestParallelDeploymentUploadsDoNotReconcileOutputFailure(t *testing.T) {
	directory, digests := parallelUploadFixture(t, 2)
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{BundleDigest: directory.Digest, Uploads: []api.DeploymentBundleUpload{fixtureUpload(server.URL, digests[0]), fixtureUpload(server.URL, digests[1])}})
	}))
	defer server.Close()
	controlPlane, err := client.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	outputErr := errors.New("output closed")
	err = uploadDeploymentBundleObjects(t.Context(), controlPlane, directory, client.EnvironmentScopeOptions{}, false, failedUploadReporter{err: outputErr})
	if !errors.Is(err, outputErr) || requests.Load() != 1 {
		t.Fatalf("error=%v requests=%d", err, requests.Load())
	}
}

func TestParallelDeploymentUploadsJoinOnCancellation(t *testing.T) {
	directory, digests := parallelUploadFixture(t, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, 2)
	exited := make(chan struct{}, 2)
	var plans atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/deployment-bundles/upload-plan" {
			plans.Add(1)
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{BundleDigest: directory.Digest, Uploads: []api.DeploymentBundleUpload{fixtureUpload(server.URL, digests[0]), fixtureUpload(server.URL, digests[1])}})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		exited <- struct{}{}
	}))
	defer server.Close()
	controlPlane, err := client.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	command.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() {
		done <- uploadDeploymentBundleObjects(ctx, controlPlane, directory, client.EnvironmentScopeOptions{}, false, newDeployReporter(command, false))
	}()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for range 2 {
		select {
		case <-entered:
		case <-timeout.C:
			cancel()
			t.Fatal("uploads did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-timeout.C:
		t.Fatal("uploads did not join")
	}
	for range 2 {
		select {
		case <-exited:
		case <-timeout.C:
			t.Fatal("request still active")
		}
	}
	if plans.Load() != 1 {
		t.Fatal("cancellation caused reconciliation")
	}
}

// Model a connection-limited link with the observed 117:69 object-size ratio.
// This measures overlap, not the bandwidth of a particular hosted environment.
func BenchmarkDeploymentUploadConnectionLimited(b *testing.B) {
	for _, parallel := range []bool{false, true} {
		name := "serial"
		if parallel {
			name = "parallel"
		}
		b.Run(name, func(b *testing.B) {
			directory, digests := parallelUploadFixture(b, 2)
			sizes := []int{117 * 1024, 69 * 1024}
			for i, digest := range digests {
				if err := os.WriteFile(directory.Objects[digest], bytes.Repeat([]byte{byte(i)}, sizes[i]), 0600); err != nil {
					b.Fatal(err)
				}
			}
			var server *httptest.Server
			var uploads []api.DeploymentBundleUpload
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/deployment-bundles/upload-plan" {
					_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{BundleDigest: directory.Digest, Uploads: uploads})
					return
				}
				buffer := make([]byte, 16*1024)
				for {
					n, err := io.ReadFull(r.Body, buffer)
					if n > 0 {
						time.Sleep(2 * time.Millisecond)
					}
					if err != nil {
						break
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			for i, digest := range digests {
				upload := fixtureUpload(server.URL, digest)
				upload.Headers["Content-Length"] = fmt.Sprint(sizes[i])
				uploads = append(uploads, upload)
			}
			controlPlane, err := client.New(server.URL)
			if err != nil {
				b.Fatal(err)
			}
			command := newRootCommand()
			command.SetErr(io.Discard)
			reporter := newDeployReporter(command, false)
			b.SetBytes(int64(sizes[0] + sizes[1]))
			for b.Loop() {
				if parallel {
					err = uploadDeploymentBundleObjects(b.Context(), controlPlane, directory, client.EnvironmentScopeOptions{}, false, reporter)
				} else {
					var plan []api.DeploymentBundleUpload
					plan, err = planDeploymentBundleObjectUploads(b.Context(), controlPlane, directory, client.EnvironmentScopeOptions{}, false)
					if err == nil {
						for _, upload := range plan {
							err = controlPlane.UploadDeploymentBundleObject(b.Context(), upload, directory.Objects[upload.Digest], nil)
							if err != nil {
								break
							}
						}
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
