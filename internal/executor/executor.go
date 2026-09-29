package executor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var ErrDetached = errors.New("runtime detached after checkpoint")

type Executor struct {
	// RunLeases serves lease claim and finalization only. Calls made on behalf
	// of a running task use the task runner's ControlPlane.Leases.
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
