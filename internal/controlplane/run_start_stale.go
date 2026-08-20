package controlplane

type runStartFailurePoint string

const (
	runStartFailureLocators             runStartFailurePoint = "locators"
	runStartFailureMode                 runStartFailurePoint = "mode"
	runStartFailureRun                  runStartFailurePoint = "run"
	runStartFailureWorkspace            runStartFailurePoint = "workspace"
	runStartFailureAttempt              runStartFailurePoint = "attempt"
	runStartFailureWorkerGroup          runStartFailurePoint = "worker_group"
	runStartFailureWorker               runStartFailurePoint = "worker"
	runStartFailureRuntime              runStartFailurePoint = "runtime"
	runStartFailureRunLease             runStartFailurePoint = "run_lease"
	runStartFailurePhysicalAuthority    runStartFailurePoint = "physical_authority"
	runStartFailureParentWait           runStartFailurePoint = "parent_wait"
	runStartFailureWorkspaceMount       runStartFailurePoint = "workspace_mount"
	runStartFailureWorkspaceLease       runStartFailurePoint = "workspace_lease"
	runStartFailureWorkspaceAuthority   runStartFailurePoint = "workspace_authority"
	runStartFailureEnclosingWait        runStartFailurePoint = "enclosing_wait"
	runStartFailureWait                 runStartFailurePoint = "wait"
	runStartFailureCheckpoint           runStartFailurePoint = "checkpoint"
	runStartFailureCheckpointValidation runStartFailurePoint = "checkpoint_validation"
	runStartFailureCheckpointSource     runStartFailurePoint = "checkpoint_source"
	runStartFailureSourceValidation     runStartFailurePoint = "source_validation"
	runStartFailureRestoreBinding       runStartFailurePoint = "restore_binding"
	runStartFailureArm                  runStartFailurePoint = "arm"
	runStartFailureLeaseState           runStartFailurePoint = "lease_state"
	runStartFailureMarkLeaseRunning     runStartFailurePoint = "mark_lease_running"
	runStartFailureMarkRunRunning       runStartFailurePoint = "mark_run_running"
	runStartFailureTouchWorkspace       runStartFailurePoint = "touch_workspace"
)
