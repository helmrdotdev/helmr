package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

var ErrDetached = errors.New("runtime detached after checkpoint")

func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

type Executor struct {
	RunLeases     RunLeaseControl
	RunLeaseTasks RunLeaseTaskRunner
}

type WaitHandler interface {
	Wait(context.Context, WaitRequest) error
}

type RunWaitAppender interface {
	AddRunWait(context.Context, WaitRequest) (api.WorkerCreateRunWaitResponse, error)
}

type WaitRequest struct {
	Leases                        api.WorkerRunLeaseProvider
	Lease                         api.WorkerRunLease
	LeaseAssignment               api.WorkerRunLeaseAssignment
	CorrelationID                 string
	RunWaitID                     string
	ResumeAttachID                string
	Kind                          api.WorkerRunWaitKind
	Params                        json.RawMessage
	Metadata                      json.RawMessage
	Tags                          []string
	TimeoutMS                     *int64
	IdleTimeoutMS                 *int64
	ActorSpeculativeInputSequence *int64
	ActiveDuration                time.Duration
	Workspace                     api.WorkerWorkspace
	Checkpointer                  Checkpointer
	Resume                        func(context.Context, WaitResumeDecision) error
}

type WaitResumeDecision struct {
	Kind string
	Data json.RawMessage
}

type Checkpointer interface {
	CreateCheckpoint(context.Context, CheckpointRequest) (CheckpointResult, error)
}

type HandoffCheckpointer interface {
	CreateHandoffCheckpoint(
		context.Context,
		CheckpointRequest,
		api.WorkerCheckpointWorkspaceBase,
	) (api.WorkerCheckpointManifest, error)
}

type CheckpointRequest struct {
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
	Manifest         api.WorkerCheckpointManifest
	WorkspaceCapture *CheckpointWorkspaceCapture
}

type CheckpointWorkspaceCapture struct {
	Tree     workspace.TreeIdentity
	Artifact workspace.WorkspaceArtifact
}
