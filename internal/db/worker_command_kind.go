package db

const (
	WorkerCommandKindRuntimePrepare          = "runtime_prepare"
	WorkerCommandKindRunResumeWait           = "run_resume_wait"
	WorkerCommandKindRunCheckpointWait       = "run_checkpoint_wait"
	WorkerCommandKindRuntimeStop             = "runtime_stop"
	WorkerCommandKindRuntimeSubstratePrepare = "runtime_substrate_prepare"
)
