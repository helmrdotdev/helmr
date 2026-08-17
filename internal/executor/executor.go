package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

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
}

type HandoffCheckpointer interface {
	CreateHandoffCheckpoint(
		context.Context,
		CheckpointRequest,
		workerapi.CheckpointWorkspaceBase,
	) (workerapi.CheckpointManifest, error)
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
	RetainSource             bool
}

type CheckpointResult struct {
	Manifest         workerapi.CheckpointManifest
	WorkspaceCapture *CheckpointWorkspaceCapture
	SourceCleanup    *workerapi.RuntimeCleanupProof
}

type CheckpointWorkspaceCapture struct {
	Tree     workspace.TreeIdentity
	Artifact workspace.WorkspaceArtifact
}
