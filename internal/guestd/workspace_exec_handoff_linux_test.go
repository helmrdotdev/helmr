//go:build linux

package guestd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

func TestWorkspaceBasicExecCommittedCaptureExecutesRetainedBytes(t *testing.T) {
	entry, registry, authority := testLinuxWorkspaceExecImage(t)
	before, err := os.ReadFile(filepath.Join(entry.workspaceRoot, "file"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
	req := successorExec(authority)
	body, err := json.Marshal(workspaceBasicExecSpec{Command: []string{"/bin/check", "-test.run=^TestWorkspaceBasicExecRetainedBytesHelper$"}, Cwd: "/workspace", Env: map[string]string{"WORKSPACE_EXEC_BYTES_HELPER": "1"}, TimeoutMS: 10000})
	if err != nil {
		t.Fatal(err)
	}
	req.RequestJson = string(body)
	result := framedBasicExec(t, t.Context(), registry, req)
	if result.GetOutcome() != "exited" || result.GetExitCode() != 0 || string(result.GetStdout()) != hex.EncodeToString(digest[:]) {
		t.Fatalf("actual confined exec: %v stdout=%q stderr=%q", result, string(result.GetStdout()), string(result.GetStderr()))
	}
	replay := framedBasicExec(t, t.Context(), registry, req)
	if replay.GetOutcome() != "exited" || string(replay.GetStdout()) != string(result.GetStdout()) {
		t.Fatal("retained result changed")
	}
}

func testLinuxWorkspaceExecImage(t *testing.T) (*workspaceMountEntry, *workspaceOperationRegistry, *workspacev0.WorkspaceRunAuthority) {
	t.Helper()
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespaces; set HELMR_PRIVILEGED_PROGRAM_TEST=1")
	}
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	image := filepath.Dir(entry.workspaceRoot)
	if err := os.MkdirAll(filepath.Join(image, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(image, "tmp"), 0777); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(image, "bin", "check"), binary, 0755); err != nil {
		t.Fatal(err)
	}
	entry.imageRoot = image
	entry.workspaceMount = "/workspace"
	entry.runtimeUser = &resolvedRuntimeUser{UID: 0, GID: 0, Home: "/tmp"}

	return entry, registry, authority
}

func TestWorkspaceBasicExecRetainedBytesHelper(t *testing.T) {
	if os.Getenv("WORKSPACE_EXEC_BYTES_HELPER") != "1" {
		return
	}
	if os.Getenv("WORKSPACE_EXEC_CHILD") == "1" {
		file, err := os.OpenFile("/workspace/child-lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			os.Exit(4)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			os.Exit(5)
		}
		if _, err := os.Stdout.Write([]byte("L")); err != nil {
			os.Exit(6)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	mode := os.Getenv("WORKSPACE_EXEC_MODE")
	if mode != "" {
		child := exec.Command("/bin/check", "-test.run=^TestWorkspaceBasicExecRetainedBytesHelper$")
		child.Env = append(os.Environ(), "WORKSPACE_EXEC_CHILD=1")
		out, err := child.StdoutPipe()
		if err != nil {
			os.Exit(7)
		}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(8)
		}
		var ready [1]byte
		if _, err := io.ReadFull(out, ready[:]); err != nil {
			os.Exit(9)
		}
		switch mode {
		case "timeout":
			time.Sleep(time.Hour)
		case "output":
			block := make([]byte, 65536)
			for {
				if _, err := os.Stdout.Write(block); err != nil {
					os.Exit(0)
				}
			}
		}
		os.Exit(0)
	}
	data, err := os.ReadFile("/workspace/file")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Hash before any write, inside the real BasicExec PID/mount namespace.
	sum := sha256.Sum256(data)
	if _, err := fmt.Fprint(os.Stdout, hex.EncodeToString(sum[:])); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestWorkspaceBasicExecRechecksAuthorityAfterPruneIO(t *testing.T) {
	for _, cancelClaim := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelClaim), func(t *testing.T) {
			entry, registry, authority := testWorkspaceFinalizationMount(t)
			runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
			journalPath := filepath.Join(entry.finalizationRoot, workspaceFinalizationJournalName)
			body, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(journalPath); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(journalPath, 0600); err != nil {
				t.Fatal(err)
			}
			req := successorExec(authority)
			if !cancelClaim {
				req.Envelope.OperationExpiresAtUnixNano = time.Now().Add(time.Second).UnixNano()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
			go func() { result <- registry.runWorkspaceBasicExec(ctx, entry, req) }()
			first, err := os.OpenFile(journalPath, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Two real FIFO opens distinguish validation IO from pruning IO without a
			// production fault-injection hook. Replace the name while the first is open.
			if err := os.Remove(journalPath); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(journalPath, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := first.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			second, err := os.OpenFile(journalPath, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			// The registry remains usable while this entry is blocked in filesystem IO.
			unrelated := &workspaceMountEntry{workspaceID: "other", channelToken: "other", fencingGeneration: 1}
			registry.register("other", unrelated)
			if _, release, ok := registry.acquireExact("other", "other", "other", 1); !ok {
				t.Fatal("unrelated registry blocked")
			} else {
				release()
			}
			if cancelClaim {
				cancel()
			} else {
				time.Sleep(time.Until(time.Unix(0, req.GetEnvelope().GetOperationExpiresAtUnixNano())))
			}
			if _, err := second.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			if response := <-result; response.GetOutcome() == "exited" {
				t.Fatal(response)
			}
			if !entry.recoveryRequired || entry.basicExec != nil || entry.authority == nil {
				t.Fatal("expired/cancelled post-prune authority did not fail closed")
			}
		})
	}
}

func TestWorkspaceBasicExecProcessContainment(t *testing.T) {
	for _, mode := range []string{"descendant", "timeout", "output"} {
		t.Run(mode, func(t *testing.T) {
			entry, registry, authority := testLinuxWorkspaceExecImage(t)
			runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
			req := successorExec(authority)
			body, err := json.Marshal(workspaceBasicExecSpec{Command: []string{"/bin/check", "-test.run=^TestWorkspaceBasicExecRetainedBytesHelper$"}, Cwd: "/workspace", Env: map[string]string{"WORKSPACE_EXEC_BYTES_HELPER": "1", "WORKSPACE_EXEC_MODE": mode}, TimeoutMS: 2000})
			if err != nil {
				t.Fatal(err)
			}
			req.RequestJson = string(body)
			response := framedBasicExec(t, t.Context(), registry, req)
			want := map[string]string{"descendant": "exited", "timeout": "workspace_exec_timed_out", "output": "workspace_exec_output_limit_exceeded"}[mode]
			if response.GetOutcome() != want {
				t.Fatalf("outcome=%s want=%s stderr=%q", response.GetOutcome(), want, response.GetStderr())
			}
			// The child held this lock until killed. A released lock after the response
			// verifies descendant cleanup without relying on translated host PID values.
			lock, err := os.OpenFile(filepath.Join(entry.workspaceRoot, "child-lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("descendant retained lock after completion: %v", err)
			}
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
		})
	}
}
