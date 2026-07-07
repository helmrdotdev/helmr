package db

const (
	WorkerCommandKindRuntimePrepare          = "runtime_prepare"
	WorkerCommandKindRuntimeResumeWait       = "runtime_resume_wait"
	WorkerCommandKindRuntimeCheckpointWait   = "runtime_checkpoint_wait"
	WorkerCommandKindRuntimeStop             = "runtime_stop"
	WorkerCommandKindRuntimeSubstratePrepare = "runtime_substrate_prepare"
)
