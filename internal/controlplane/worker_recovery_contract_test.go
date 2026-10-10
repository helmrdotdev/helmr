package controlplane

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Exercise production recovery and Supervisor request construction against the
// actual Control Plane validator, rather than independently authored DTOs.
func TestWorkerRecoveryEvidenceSatisfiesStartupContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production recovery requires Linux procfs")
	}
	for _, name := range []string{"reclaimed", "network", "cgroup", "process", "process-two"} {
		t.Run(name, func(t *testing.T) {
			quarantine := name != "reclaimed"
			work, jailer := t.TempDir(), t.TempDir()
			owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
			state := filepath.Join(work, "vms", "guest", owner.ID)
			if err := os.MkdirAll(state, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var children []*exec.Cmd
			if name == "process" || name == "process-two" {
				count := 1
				if name == "process-two" {
					count = 2
				}
				for range count {
					children = append(children, startRecoveryContradictoryProcess(t, owner.ID))
				}
			}
			ip, err := exec.LookPath("true")
			if err != nil {
				t.Fatal(err)
			}
			reachedActivation := errors.New("validated recovery reached activation")
			cp := &startupRecoveryContractCP{stop: reachedActivation}
			cgroupChecked := false
			supervisor, err := worker.New(worker.Config{ControlPlane: cp, Capabilities: workerapi.Capabilities{ExecutionSlotsAvailable: 2}, Recover: func(ctx context.Context) (worker.RecoveryEvidence, error) {
				return worker.RecoverLocalVMState(ctx, work, jailer, ip, func(_ context.Context, got vm.Owner) error {
					if got != owner {
						t.Errorf("wrong cleanup owner: %+v", got)
					}
					if name == "network" {
						return errors.New("network cleanup remains busy")
					}
					return nil
				}, func(got vm.Owner) error {
					if got != owner {
						t.Errorf("wrong cgroup owner: %+v", got)
					}
					cgroupChecked = true
					if name == "cgroup" {
						return errors.New("cgroup cleanup remains busy")
					}
					return nil
				})
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := supervisor.Run(ctx); !errors.Is(err, reachedActivation) {
				t.Fatalf("startup rejected real recovery evidence: %v", err)
			}
			for _, child := range children {
				command, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child.Process.Pid), "cmdline"))
				if err != nil || len(command) == 0 {
					t.Fatalf("quarantine stopped live child: %v", err)
				}
			}
			if cgroupChecked != (name == "reclaimed" || name == "cgroup") {
				t.Fatal("cgroup cleanup did not respect preceding network failure")
			}
			want := []string{owner.ID}

			if quarantine {
				if _, err := os.Stat(filepath.Join(state, "owner")); err != nil {
					t.Fatalf("quarantine lost ownership evidence: %v", err)
				}
				if !reflect.DeepEqual(cp.request.Quarantined, want) || cp.slots != 1 {
					t.Fatalf("quarantine=%+v slots=%d", cp.request, cp.slots)
				}
			} else if len(cp.request.Quarantined) != 0 || cp.slots != 2 {
				t.Fatalf("reclaimed=%+v slots=%d", cp.request, cp.slots)
			}
		})
	}
}

type startupRecoveryContractCP struct {
	worker.ControlPlane
	request workerapi.StartupRecoveryRequest
	slots   int32
	stop    error
}

func (*startupRecoveryContractCP) AuthenticateWorker(context.Context) error { return nil }
func (c *startupRecoveryContractCP) ReportWorkerStartupRecovery(_ context.Context, q workerapi.StartupRecoveryRequest) error {
	if err := validateWorkerStartupRecovery(q); err != nil {
		return &httpclient.Error{StatusCode: 400, Message: err.Error()}
	}
	c.request = q
	return nil
}
func (c *startupRecoveryContractCP) ActivateWorker(_ context.Context, q workerapi.Capabilities) (workerapi.StatusResponse, error) {
	c.slots = q.ExecutionSlotsAvailable
	return workerapi.StatusResponse{}, c.stop
}

// The child deliberately names an owner but retains the host root. Recovery
// must report the owner as quarantined without signaling either child.
func TestWorkerRecoveryContradictoryProcessChild(t *testing.T) {
	if os.Getenv("HELMR_RECOVERY_CONTRACT_CHILD") != "1" {
		return
	}
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte("ready")); err != nil {
		os.Exit(2)
	}
	if err := ready.Close(); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func startRecoveryContradictoryProcess(t *testing.T, id string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command(binary, "-test.run=^TestWorkerRecoveryContradictoryProcessChild$", "--", "--id", id)
	cmd.Args[0] = "/firecracker"
	cmd.Env = append(os.Environ(), "HELMR_RECOVERY_CONTRACT_CHILD=1")
	cmd.ExtraFiles = []*os.File{w}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready, err := io.ReadAll(r)
	if err != nil || string(ready) != "ready" {
		t.Fatalf("child readiness=%q: %v", ready, err)
	}
	return cmd
}
