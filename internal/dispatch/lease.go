package dispatch

import "time"

type Lease struct {
	ID               string
	MessageID        string
	Message          Message
	WorkerInstanceID string
	SessionID        string
	AttemptNumber    int32
	ExpiresAt        time.Time
}
