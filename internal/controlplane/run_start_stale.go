package controlplane

import "errors"

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
	runStartFailureParentHandoff        runStartFailurePoint = "parent_handoff"
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

type staleRunStartError struct {
	point runStartFailurePoint
}

func (e *staleRunStartError) Error() string {
	return errStaleRunLeaseClaim.Error()
}

func (e *staleRunStartError) Unwrap() error {
	return errStaleRunLeaseClaim
}

func staleRunStart(point runStartFailurePoint, err error) error {
	if err == nil || !errors.Is(err, errStaleRunLeaseClaim) {
		return err
	}
	var existing *staleRunStartError
	if errors.As(err, &existing) {
		return err
	}
	return &staleRunStartError{point: point}
}

func runStartFailurePointOf(err error) (runStartFailurePoint, bool) {
	var stale *staleRunStartError
	if !errors.As(err, &stale) {
		return "", false
	}
	return stale.point, true
}
