package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func testMachineKey() []byte {
	return bytes.Repeat([]byte{7}, ed25519.SeedSize)
}

func TestReadWorkerEnrollmentToken(t *testing.T) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker-token")
	if err := os.WriteFile(path, []byte(token.Raw), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := readWorkerEnrollmentToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != token.Raw {
		t.Fatalf("token = %q", got)
	}
}

func TestReadWorkerEnrollmentTokenRejectsUnsafeFiles(t *testing.T) {
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte(token.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	wrongMode := filepath.Join(dir, "wrong-mode")
	if err := os.WriteFile(wrongMode, []byte(token.Raw), 0o644); err != nil {
		t.Fatal(err)
	}
	newline := filepath.Join(dir, "newline")
	if err := os.WriteFile(newline, []byte(token.Raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{symlink, wrongMode, newline, large} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := readWorkerEnrollmentToken(path); err == nil {
				t.Fatal("unsafe token file accepted")
			}
		})
	}
}

// A control plane on another worker API version rejects the stored identity's
// first host credential request, its challenge, with a 409, not a 401: the worker keeps its stored
// identity and neither re-enrolls nor retries.
func TestWorkerIdentitySurvivesAPIVersionMismatch(t *testing.T) {
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"version mismatch"}}`, workerapi.APIVersionMismatchCode)
	}))
	defer server.Close()
	workDir := t.TempDir()
	workDir, jailerDir, err := resolveWorkerRecoveryRoots(workDir, "")
	if err != nil {
		t.Fatal(err)
	}
	stored := workerIdentityFile{WorkDir: workDir, JailerDir: jailerDir, WorkerHostID: "host", MachineKey: testMachineKey(), CreatedAt: time.Now().UTC()}
	path := workerIdentityPath(workDir, "")
	if err := writeWorkerIdentity(path, stored); err != nil {
		t.Fatal(err)
	}
	cfg := config.Worker{ControlPlaneURL: server.URL}
	_, err = resolveAuthenticatedWorkerIdentity(t.Context(), cfg, workDir, func(identity workerIdentityFile) error {
		client, err := workerclient.New(server.URL, workerclient.WithHTTPClient(server.Client()),
			workerclient.WithAuth(identity.WorkerHostID, identity.key()), workerclient.WithService("service"))
		if err != nil {
			return err
		}
		return client.AuthenticateWorker(t.Context())
	})
	var mismatch *httpclient.Error
	if !errors.As(err, &mismatch) || mismatch.Code != workerapi.APIVersionMismatchCode {
		t.Fatalf("error = %v, want version mismatch", err)
	}
	if kept, err := readWorkerIdentity(path); err != nil || kept.WorkerHostID != stored.WorkerHostID || !bytes.Equal(kept.MachineKey, stored.MachineKey) {
		t.Fatalf("stored identity = %+v, err = %v; want it kept", kept, err)
	}
	if len(requests) != 1 || requests["/worker/v1/instance/challenge"] != 1 {
		t.Fatalf("requests = %v, want one host challenge request", requests)
	}
}

func TestWorkerIdentityBindsRecoveryRootsBeforeAuthentication(t *testing.T) {
	for _, change := range []string{"work", "jailer", "missing", "alias", "retarget"} {
		t.Run(change, func(t *testing.T) {
			base := t.TempDir()
			work, jailer, err := resolveWorkerRecoveryRoots(filepath.Join(base, "work"), "")
			if err != nil {
				t.Fatal(err)
			}
			if jailer != filepath.Join(work, "vms", "jailer") {
				t.Fatal("incorrect default jailer root")
			}
			identity := workerIdentityFile{WorkerHostID: "host", MachineKey: testMachineKey(), WorkDir: work, JailerDir: jailer}
			cfg := config.Worker{WorkerIdentityPath: filepath.Join(base, "credential"), JailerChrootDir: jailer}
			switch change {
			case "work":
				work = filepath.Join(base, "other-work")
			case "jailer":
				cfg.JailerChrootDir = filepath.Join(base, "other-jailer")
			case "missing":
				identity.WorkDir, identity.JailerDir = "", ""
			case "alias", "retarget":
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(work, alias); err != nil {
					t.Fatal(err)
				}
				work = alias
				if change == "retarget" {
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(base, "other-work"), alias); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(base, "other-work"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writeWorkerIdentity(cfg.WorkerIdentityPath, identity); err != nil {
				t.Fatal(err)
			}
			called := false
			_, err = resolveAuthenticatedWorkerIdentity(t.Context(), cfg, work, func(workerIdentityFile) error { called = true; return nil })
			if change == "alias" {
				if err != nil || !called {
					t.Fatalf("equivalent alias rejected: %v", err)
				}
			} else if err == nil || called {
				t.Fatalf("root change authenticated: called=%v err=%v", called, err)
			}
			kept, err := readWorkerIdentity(cfg.WorkerIdentityPath)
			if err != nil || !reflect.DeepEqual(kept, identity) {
				t.Fatal("binding rejection modified stored credential")
			}
		})
	}
}

// A lost enrollment response leaves the machine key on disk, so the next
// attempt enrolls the same key and the Control Plane returns the same host.
func TestWorkerEnrollmentRetriesWithTheStoredKey(t *testing.T) {
	var keys [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request workerapi.EnrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		keys = append(keys, request.PublicKey)
		w.Header().Set("content-type", "application/json")
		if len(keys) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"lost"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"worker_host_id":"host","worker_group_id":%q,"worker_pool_id":%q}`, uuid.NewV7(), uuid.NewV7())
	}))
	defer server.Close()
	token, err := auth.GenerateSingleUseEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(dir, "work")
	cfg := config.Worker{ControlPlaneURL: server.URL, WorkerEnrollmentTokenFile: tokenFile, WorkerResourceID: "laptop", WorkerPoolName: "run"}
	if _, err := resolveWorkerIdentity(t.Context(), cfg, workDir); err == nil {
		t.Fatal("enrollment succeeded on a lost response")
	}
	identity, err := resolveWorkerIdentity(t.Context(), cfg, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !bytes.Equal(keys[0], keys[1]) || !bytes.Equal(keys[1], identity.key().Public().(ed25519.PublicKey)) {
		t.Fatalf("enrolled keys = %x, stored key = %x", keys, identity.key().Public())
	}
	if _, err := os.Stat(pendingMachineKeyPath(workerIdentityPath(identity.WorkDir, ""))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending key left behind: %v", err)
	}
}

// A pending key that the Control Plane already registered, for a host since
// fenced or revoked, is replaced by a new key instead of wedging enrollment.
func TestWorkerEnrollmentReplacesARegisteredPendingKey(t *testing.T) {
	var keys [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request workerapi.EnrollmentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		keys = append(keys, request.PublicKey)
		w.Header().Set("content-type", "application/json")
		if len(keys) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"worker_key_registered","message":"registered"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"worker_host_id":"host","worker_group_id":%q,"worker_pool_id":%q}`, uuid.NewV7(), uuid.NewV7())
	}))
	defer server.Close()
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Worker{ControlPlaneURL: server.URL, WorkerEnrollmentTokenFile: tokenFile, WorkerResourceID: "machine", WorkerPoolName: "run"}
	identity, err := resolveWorkerIdentity(t.Context(), cfg, filepath.Join(dir, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || bytes.Equal(keys[0], keys[1]) || !bytes.Equal(keys[1], identity.key().Public().(ed25519.PublicKey)) {
		t.Fatalf("enrolled keys = %x, stored key = %x", keys, identity.key().Public())
	}
}

// A pending key left by a crash after the identity was stored is removed, so
// a later re-enrollment cannot reuse the identity's key.
func TestWorkerIdentityRemovesALeftoverPendingKey(t *testing.T) {
	workDir, jailerDir, err := resolveWorkerRecoveryRoots(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	path := workerIdentityPath(workDir, "")
	stored := workerIdentityFile{WorkerHostID: "host", MachineKey: testMachineKey(), WorkDir: workDir, JailerDir: jailerDir}
	if err := writeWorkerIdentity(path, stored); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerIdentity(pendingMachineKeyPath(path), workerIdentityFile{MachineKey: testMachineKey()}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkerIdentity(t.Context(), config.Worker{}, workDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingMachineKeyPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover pending key kept: %v", err)
	}
}
