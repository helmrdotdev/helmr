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

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

func TestComputerBasicExecExecutesRetainedBytes(t *testing.T) {
	entry, registry, _ := testLinuxComputerCommandImage(t)
	before, err := os.ReadFile(filepath.Join(entry.computerRoot, "file"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	req := testComputerBasicExecRequest("process-1", "fingerprint")
	req.Secrets = []*computerv0.ComputerSecretDelivery{{PlacementKind: "file", PlacementTarget: "/secrets/pinned-token", Value: []byte("pinned-value")}}
	body, err := json.Marshal(computerBasicExecSpec{Command: []string{"/bin/check", "-test.run=^TestComputerBasicExecRetainedBytesHelper$"}, Cwd: "/computer", Env: map[string]string{"COMPUTER_EXEC_BYTES_HELPER": "1"}, TimeoutMS: 10000})
	if err != nil {
		t.Fatal(err)
	}
	req.RequestJson = string(body)
	result := framedBasicExec(t, t.Context(), registry, req)
	if result.GetOutcome() != "exited" || result.GetExitCode() != 0 || string(commandSpoolBytes(t, entry.commands["process-1"].output, "stdout")) != hex.EncodeToString(digest[:]) {
		t.Fatalf("actual confined exec: %v stdout=%q stderr=%q", result, string(commandSpoolBytes(t, entry.commands["process-1"].output, "stdout")), string(commandSpoolBytes(t, entry.commands["process-1"].output, "stderr")))
	}
	replay := framedBasicExec(t, t.Context(), registry, req)
	if replay.GetOutcome() != "exited" || string(commandSpoolBytes(t, entry.commands["process-1"].output, "stdout")) != hex.EncodeToString(digest[:]) {
		t.Fatal("retained result changed")
	}
}

func testLinuxComputerCommandImage(t *testing.T) (*computerMountEntry, *computerOperationRegistry, *computerv0.ComputerRunAuthority) {
	t.Helper()
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux namespaces; set HELMR_PRIVILEGED_PROGRAM_TEST=1")
	}
	entry, registry, authority := testProgramMount(t)
	image := filepath.Dir(entry.computerRoot)
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
	entry.computerInstanceID = "instance-1"
	entry.writerGeneration = 1
	entry.channelToken = "channel-token"
	entry.imageRoot = image
	entry.computerMount = "/computer"
	entry.runtimeUser = &resolvedRuntimeUser{UID: 0, GID: 0, Home: "/tmp"}

	return entry, registry, authority
}

func TestComputerBasicExecRetainedBytesHelper(t *testing.T) {
	if os.Getenv("COMPUTER_EXEC_BYTES_HELPER") != "1" {
		return
	}
	if os.Getenv("COMPUTER_EXEC_MODE") == "" {
		value, err := os.ReadFile("/secrets/pinned-token")
		if err != nil || string(value) != "pinned-value" {
			os.Exit(9)
		}
	}
	if os.Getenv("COMPUTER_EXEC_CHILD") == "1" {
		file, err := os.OpenFile("/computer/child-lock", os.O_CREATE|os.O_RDWR, 0600)
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
	mode := os.Getenv("COMPUTER_EXEC_MODE")
	if mode != "" {
		child := exec.Command("/bin/check", "-test.run=^TestComputerBasicExecRetainedBytesHelper$")
		child.Env = append(os.Environ(), "COMPUTER_EXEC_CHILD=1")
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
		case "cancel":
			fmt.Fprint(os.Stdout, "ready")
			time.Sleep(time.Hour)
		case "timeout":
			time.Sleep(time.Hour)
		case "output":
			block := make([]byte, 65536)
			for range 128 {
				if _, err := os.Stdout.Write(block); err != nil {
					os.Exit(0)
				}
			}
		}
		os.Exit(0)
	}
	data, err := os.ReadFile("/computer/file")
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

func TestComputerBasicExecProcessContainment(t *testing.T) {
	for _, mode := range []string{"descendant", "timeout", "output", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entry, registry, _ := testLinuxComputerCommandImage(t)
			req := testComputerBasicExecRequest("process-1", "fingerprint")
			body, err := json.Marshal(computerBasicExecSpec{Command: []string{"/bin/check", "-test.run=^TestComputerBasicExecRetainedBytesHelper$"}, Cwd: "/computer", Env: map[string]string{"COMPUTER_EXEC_BYTES_HELPER": "1", "COMPUTER_EXEC_MODE": mode}, TimeoutMS: 2000})
			if err != nil {
				t.Fatal(err)
			}
			req.RequestJson = string(body)
			if mode == "cancel" {
				var spec computerBasicExecSpec
				if err := json.Unmarshal(body, &spec); err != nil {
					t.Fatal(err)
				}
				spec.TimeoutMS = 60000
				body, err = json.Marshal(spec)
				if err != nil {
					t.Fatal(err)
				}
				req.RequestJson = string(body)
				execution, _, err := registry.startComputerBasicExec(t.Context(), entry, req)
				if err != nil {
					t.Fatal(err)
				}
				defer execution.cancel()
				var offset int64
				readyCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
				defer stop()
				for {
					chunk, next, changed, err := execution.output.read(offset)
					if err != nil {
						t.Fatal(err)
					}
					if chunk != nil {
						offset = next
						if chunk.Stream == "stdout" && string(chunk.Content) == "ready" {
							break
						}
						continue
					}
					select {
					case <-changed:
					case <-execution.done:
						t.Fatalf("command ended before cancellation: %v", execution.result)
					case <-readyCtx.Done():
						t.Fatal(readyCtx.Err())
					}
				}
				if err := registry.cancelCommand(t.Context(), req.Envelope); err != nil {
					t.Fatal(err)
				}
			}
			response := framedBasicExec(t, t.Context(), registry, req)
			want := map[string]string{"descendant": "exited", "timeout": "computer_command_timed_out", "output": "exited", "cancel": "computer_command_cancelled"}[mode]
			if response.GetOutcome() != want {
				t.Fatalf("outcome=%s want=%s stderr=%q", response.GetOutcome(), want, commandSpoolBytes(t, entry.commands["process-1"].output, "stderr"))
			}
			// The child held this lock until killed. A released lock after the response
			// verifies descendant cleanup without relying on translated host PID values.
			lock, err := os.OpenFile(filepath.Join(entry.computerRoot, "child-lock"), os.O_RDWR, 0)
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
