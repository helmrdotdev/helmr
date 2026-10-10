package workerapi

import "time"

// CommandLogAppendRequest identifies the producing Command and Computer lease.
// Retrying a chunk preserves its sequence, timestamp and bytes.
type CommandLogAppendRequest struct {
	EnvironmentID      string    `json:"environment_id"`
	CommandID          string    `json:"command_id"`
	ComputerInstanceID string    `json:"computer_instance_id"`
	WriterGeneration   int64     `json:"writer_generation"`
	Stream             LogStream `json:"stream"`
	ObservedSeq        uint64    `json:"observed_seq"`
	ObservedAt         time.Time `json:"observed_at"`
	Kind               string    `json:"kind"`
	ThroughSequence    uint64    `json:"through_sequence"`
	DroppedBytes       int64     `json:"dropped_bytes"`
	Complete           bool      `json:"complete"`
	Content            []byte    `json:"content"`
}
