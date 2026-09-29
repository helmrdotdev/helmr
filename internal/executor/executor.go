package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var ErrDetached = errors.New("runtime detached after checkpoint")

func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

type Executor struct {
	RunLeases     RunLeaseControlPlane
	RunLeaseTasks RunLeaseTaskRunner
}

type WaitRequest struct {
	Execution                     *programv0.SessionExecution
	TurnID                        *string
	Leases                        workerapi.RunLeaseAssignmentProvider
	Lease                         workerapi.RunLease
	LeaseAssignment               workerapi.RunLeaseAssignment
	CorrelationID                 string
	RunWaitID                     string
	ResumeAttachID                string
	Kind                          workerapi.RunWaitKind
	Params                        json.RawMessage
	Metadata                      json.RawMessage
	Tags                          []string
	TimeoutMS                     *int64
	IdleTimeoutMS                 *int64
	ActorSpeculativeInputSequence *int64
	ActiveDuration                time.Duration
	Computer                      workerapi.Computer
	Resume                        func(context.Context, WaitResumeDecision) error
}

type WaitResumeDecision struct {
	Kind string
	Data json.RawMessage
}

// ComputerCheckpointRequest belongs to the physical Instance owner. Member
// pausing is coordinated before the guest returns the whole-Computer proof.
type ComputerCheckpointRequest struct {
	Target   workerapi.RuntimeReconcileTarget
	Register func(context.Context, workerapi.CheckpointManifest) error
}

type CheckpointResult struct {
	Manifest workerapi.CheckpointManifest
}
