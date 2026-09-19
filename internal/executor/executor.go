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
	"github.com/helmrdotdev/helmr/internal/workspace"
)

var ErrDetached = errors.New("runtime detached after checkpoint")

func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

type Executor struct {
	RunLeases     RunLeaseControlPlane
	RunLeaseTasks RunLeaseTaskRunner
}

type WaitHandler interface {
	Wait(context.Context, WaitRequest) error
}

type RunWaitAppender interface {
	AddRunWait(context.Context, WaitRequest) (workerapi.CreateRunWaitResponse, error)
}

type WaitRequest struct {
	Execution                     *programv0.SessionExecution
	TurnID                        *string
	Leases                        workerapi.RunLeaseProvider
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
	Workspace                     workerapi.Workspace
	Checkpointer                  Checkpointer
	Resume                        func(context.Context, WaitResumeDecision) error
}

type WaitResumeDecision struct {
	Kind string
	Data json.RawMessage
}

type Checkpointer interface {
	CreateCheckpoint(context.Context, CheckpointRequest) (CheckpointResult, error)
	ReleaseCheckpointSource(context.Context) error
}

type CheckpointRequest struct {
	Execution                *programv0.SessionExecution
	TurnID                   *string
	RunID                    string
	AttemptNumber            int32
	RunLeaseID               string
	RunWaitID                string
	CorrelationID            string
	CheckpointID             string
	ResumeAttachID           string
	CheckpointRequestVersion int64
	CaptureWorkspace         bool
}

type CheckpointResult struct {
	Manifest         workerapi.CheckpointManifest
	WorkspaceCapture *CheckpointWorkspaceCapture
}

type CheckpointWorkspaceCapture struct {
	Tree     workspace.TreeIdentity
	Artifact workspace.WorkspaceArtifact
}
