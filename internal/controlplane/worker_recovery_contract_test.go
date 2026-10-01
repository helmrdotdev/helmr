package controlplane

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Exercise production recovery and Supervisor request construction against the
// actual Control Plane validator, rather than independently authored DTOs.
func TestWorkerRecoveryEvidenceSatisfiesStartupContract(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		name := "reclaimed"
		if quarantine {
			name = "quarantined"
		}
		t.Run(name, func(t *testing.T) {
			work, jailer := t.TempDir(), t.TempDir()
			owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019c10d5-a6f7-7af1-8f5f-000000000201"}
			state := filepath.Join(work, "vms", "guest", owner.ID)
			if err := os.MkdirAll(state, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ip, err := exec.LookPath("true")
			if err != nil {
				t.Fatal(err)
			}
			reachedActivation := errors.New("validated recovery reached activation")
			cp := &startupRecoveryContractCP{epochStartedAt: time.Now().Add(-time.Minute), stop: reachedActivation}
			supervisor, err := worker.New(worker.Config{ControlPlane: cp, Capabilities: workerapi.Capabilities{ExecutionSlotsAvailable: 2}, Recover: func(ctx context.Context) (worker.RecoveryEvidence, error) {
				return worker.RecoverLocalVMState(ctx, work, jailer, ip, func(_ context.Context, got vm.Owner) error {
					if got != owner {
						t.Errorf("wrong cleanup owner: %+v", got)
					}
					if quarantine {
						return errors.New("network cleanup remains busy")
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
			want := []string{owner.ID}
			if !reflect.DeepEqual(cp.request.Inventory, want) {
				t.Fatalf("inventory=%v", cp.request.Inventory)
			}
			if quarantine {
				if !reflect.DeepEqual(cp.request.Quarantined, want) || len(cp.request.Reclaimed) != 0 || cp.slots != 1 {
					t.Fatalf("quarantine=%+v slots=%d", cp.request, cp.slots)
				}
			} else if !reflect.DeepEqual(cp.request.Reclaimed, want) || len(cp.request.Quarantined) != 0 || cp.slots != 2 {
				t.Fatalf("reclaimed=%+v slots=%d", cp.request, cp.slots)
			}
		})
	}
}

type startupRecoveryContractCP struct {
	worker.ControlPlane
	epochStartedAt time.Time
	request        workerapi.StartupRecoveryRequest
	slots          int32
	stop           error
}

func (*startupRecoveryContractCP) AuthenticateWorker(context.Context) error { return nil }
func (c *startupRecoveryContractCP) ReportWorkerStartupRecovery(_ context.Context, q workerapi.StartupRecoveryRequest) error {
	if err := validateWorkerStartupRecovery(q, c.epochStartedAt, time.Now()); err != nil {
		return &httpclient.Error{StatusCode: 400, Message: err.Error()}
	}
	c.request = q
	return nil
}
func (c *startupRecoveryContractCP) ActivateWorker(_ context.Context, q workerapi.Capabilities) (workerapi.StatusResponse, error) {
	c.slots = q.ExecutionSlotsAvailable
	return workerapi.StatusResponse{}, c.stop
}
