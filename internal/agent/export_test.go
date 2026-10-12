package agent

import "uuid"

// Test-only access for the external test package.

type (
	SessionLifecyclePosition = sessionLifecyclePosition
	TriggerPosition          = triggerPosition
)

var (
	AdmitSession                 = admitSession
	CloseProcessing              = closeProcessing
	CreateSlackRoot              = createSlackRoot
	DigestJSON                   = digestJSON
	EventData                    = eventData
	ExpireSessionDeadline        = expireSessionDeadline
	HideMissing                  = hideMissing
	LockSession                  = lockSession
	ProcessStopTimeout           = processStopTimeout
	ReadSlackStartSource         = readSlackStartSource
	ReconcileProcessStopDeadline = reconcileProcessStopDeadline
	ReconcileSessionCompletion   = reconcileSessionCompletion
	ReconcileSessionHistory      = reconcileSessionHistory
	ReconcileSessionIdle         = reconcileSessionIdle
	ReconcileSessionLifecycle    = reconcileSessionLifecycle
	ReconcileTerminalSessionStop = reconcileTerminalSessionStop
	ReconcileTriggers            = reconcileTriggers
	RegisterTemplateDefinitions  = registerTemplateDefinitions
	RetainSessionHistory         = retainSessionHistory
)

// Session reports the Session a lifecycle scan stopped at.
func (p sessionLifecyclePosition) Session() uuid.UUID { return p.session }
