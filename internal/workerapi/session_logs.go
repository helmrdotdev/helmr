package workerapi

// SessionLogRequest is sent by the authenticated physical owner, not the authored
// Runtime. It contains no caller-selected Environment/owner overrides beyond the
// exact Session execution and attachment already verified for this transport.
type SessionLogRequest struct {
	Session            RuntimeSession `json:"session"`
	AttachmentSequence int64          `json:"attachment_sequence"`
	Stream             string         `json:"stream"`
	Kind               string         `json:"kind"`
	Sequence           int64          `json:"sequence"`
	ThroughSequence    int64          `json:"through_sequence"`
	ObservedAtUnixNano int64          `json:"observed_at_unix_nano"`
	Data               []byte         `json:"data"`
	DroppedBytes       int64          `json:"dropped_bytes"`
	Complete           bool           `json:"complete"`
}
