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
	ProcessStopTimeout           = processStopTimeout
	ReconcileProcessStopDeadline = reconcileProcessStopDeadline
	ReconcileSessionCompletion   = reconcileSessionCompletion
	ReconcileSessionIdle         = reconcileSessionIdle
	ReconcileSessionLifecycle    = reconcileSessionLifecycle
	ReconcileTerminalSessionStop = reconcileTerminalSessionStop
)

// Session reports the Session a lifecycle scan stopped at.
func (p sessionLifecyclePosition) Session() uuid.UUID { return p.session }
