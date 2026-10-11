package workerapi

import "time"

type DiagnosticLogReceipt struct {
	Expired         bool      `json:"expired"`
	ThroughSequence int64     `json:"through_sequence"`
	AcceptedAt      time.Time `json:"accepted_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}
