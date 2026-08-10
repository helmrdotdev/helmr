package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestDeployBundleUsesUploadFinalizePromoteFlow(t *testing.T) {
	directory, raw, digest, objectDigest := writeDeployTestBundle(t)
	const deploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	requests := make([]string, 0, 4)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/deployment-bundles/upload-plan":
			if r.Header.Get("Content-Type") != deployment.DeploymentBundleMediaType {
				t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, raw) {
				t.Fatal("upload plan body differs from bundle.json")
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
				BundleDigest: digest,
				Uploads: []api.DeploymentBundleUpload{{
					Digest: objectDigest, Method: http.MethodPut, URL: server.URL + "/upload",
					Headers: map[string]string{"Content-Type": deployment.ProgramArtifactMediaType},
				}},
			})
		case "/upload":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("Control Plane credential leaked to object upload")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "program" {
				t.Fatalf("upload body = %q, err = %v", body, err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/deployment-bundles/finalize":
			var request api.FinalizeDeploymentBundleRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.BundleDigest != digest || request.IdempotencyKey != "deploy-test" {
				t.Fatalf("finalize request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		case "/v1/deployments/" + deploymentID + "/promote":
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{
				ID: deploymentID, Version: "v-test", BundleDigest: digest, CreatedAt: time.Now(),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", "--bundle", directory, "--idempotency-key", "deploy-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "v-test\n" {
		t.Fatalf("output = %q", out.String())
	}
	want := []string{
		"POST /v1/deployment-bundles/upload-plan",
		"PUT /upload",
		"POST /v1/deployment-bundles/finalize",
		"POST /v1/deployments/" + deploymentID + "/promote",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %v", requests)
	}
}

func TestDeployBundleRejectsLegacyRemoteBuildFlags(t *testing.T) {
	command := deployCommand()
	for _, name := range []string{"detach", "timeout", "no-image-cache"} {
		if command.Flags().Lookup(name) != nil {
			t.Fatalf("legacy remote-build flag %q remains", name)
		}
	}
}

func TestDeployBundleRejectsUploadOutsideClosure(t *testing.T) {
	directory, _, digest, _ := writeDeployTestBundle(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/deployment-bundles/upload-plan" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.DeploymentBundleUploadPlanResponse{
			BundleDigest: digest,
			Uploads: []api.DeploymentBundleUpload{{
				Digest: "sha256:" + strings.Repeat("e", 64), Method: http.MethodPut,
				URL: "https://upload.invalid/object",
			}},
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	cmd := newRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"deploy", "--bundle", directory})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "outside the bundle") {
		t.Fatalf("error = %v", err)
	}
}

func writeDeployTestBundle(t *testing.T) (string, []byte, string, string) {
	t.Helper()
	program := []byte("program")
	programDigest := sha256sum.DigestBytes(program)
	runtimeDigest := "sha256:" + strings.Repeat("f", 64)
	declaration := deployment.ProgramIndexDeclaration{
		Kind: deployment.DefinitionKindTask, DeclaredID: "hello",
		Task: &deployment.TaskManifest{
			Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
			Run: deployment.RunManifest{Queue: "default", MaxDurationMs: 5000,
				Retry: deployment.RetryManifest{Enabled: false}},
		},
		Locator: &deployment.ProgramLocator{
			ExportName: "hello", ModulePath: ".helmr/modules/" + strings.Repeat("d", 64) + ".mjs",
			Slot: deployment.DeclarationSlotHandler,
		},
	}
	queues := []deployment.QueueInput{{Name: "default"}}
	plan := deployment.DeploymentPlan{FormatVersion: deployment.DeploymentPlanFormatVersion,
		Definitions: []deployment.ProgramIndexDeclaration{declaration}, Queues: queues}
	bundle := deployment.DeploymentBundle{
		Contract: deployment.DeploymentBundleContract,
		Platform: deployment.DeploymentBundlePlatform{Architecture: deployment.ArchitectureX8664,
			OS: deployment.DeploymentBundleTargetOS},
		Plan: plan,
		Runtime: deployment.DeploymentBundleRuntime{Contract: deployment.RuntimeContract,
			Artifact: deployment.BundleObject{Digest: runtimeDigest, SizeBytes: 4096,
				MediaType: deployment.RuntimeArtifactMediaType}},
		Program: deployment.ProgramOutput{
			Artifact: deployment.ProgramDescriptor{Digest: programDigest, SizeBytes: int64(len(program)),
				MediaType: deployment.ProgramArtifactMediaType},
			Index: deployment.ProgramIndex{Architecture: deployment.ArchitectureX8664,
				ConfigResultDigest: "sha256:" + strings.Repeat("c", 64),
				Declarations:       []deployment.ProgramIndexDeclaration{declaration}, Queues: queues,
				RuntimeContract: deployment.RuntimeContract, RuntimeDigest: runtimeDigest},
		},
		WorkspaceImages: []deployment.BundleWorkspaceImage{},
		Objects: []deployment.BundleObject{{Digest: programDigest, SizeBytes: int64(len(program)),
			MediaType: deployment.ProgramArtifactMediaType}},
	}
	raw, err := deployment.CanonicalDeploymentBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := deployment.DeploymentBundleDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	objects := filepath.Join(root, "objects", "sha256")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, strings.TrimPrefix(programDigest, "sha256:")), program, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, raw, digest, programDigest
}
