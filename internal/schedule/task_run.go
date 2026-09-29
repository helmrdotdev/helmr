package schedule

import "github.com/helmrdotdev/helmr/internal/computer"

type TaskRun struct {
	QueueName             string
	QueueConcurrencyLimit *int64
	QueuedTTLMS           *int64
	MaxActiveDurationMS   int64
	RetryPolicy           []byte
	SandboxDeclaredID     string
	SecretPlacements      []computer.SecretPlacement
}
