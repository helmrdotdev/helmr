package session

import "uuid"

// Test-only access for the external test package.

type (
	SessionLifecyclePosition = sessionLifecyclePosition
)

var (
	AdmitSession                 = admitSession
	CloseProcessing              = closeProcessing
	DigestJSON                   = digestJSON
	EventData                    = eventData
	ExpireSessionDeadline        = expireSessionDeadline
	HideMissing                  = hideMissing
	LockSession                  = lockSession
	ProcessFailureHoldThreshold  = processFailureHoldThreshold
	ProcessStopTimeout           = processStopTimeout
	ReconcileProcessStopDeadline = reconcileProcessStopDeadline
	ReconcileSessionIdle         = reconcileSessionIdle
	ReconcileSessionLifecycle    = reconcileSessionLifecycle
	ReconcileTerminalSessionStop = reconcileTerminalSessionStop
	RecordSessionFailure         = recordSessionFailure
)

// Session reports the Session a lifecycle scan stopped at.
func (p sessionLifecyclePosition) Session() uuid.UUID { return p.session }
