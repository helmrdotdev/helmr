package workerapi

import "time"

// CommandLogAppendRequest identifies the producing execution independently of a Run.
// Retrying a chunk preserves its sequence, timestamp and bytes.
type CommandLogAppendRequest struct {
	OrgID              string    `json:"org_id"`
	CommandID          string    `json:"command_id"`
	ComputerInstanceID string    `json:"computer_instance_id"`
	WriterGeneration   int64     `json:"writer_generation"`
	Stream             LogStream `json:"stream"`
	ObservedSeq        uint64    `json:"observed_seq"`
	ObservedAt         time.Time `json:"observed_at"`
	Content            []byte    `json:"content"`
}
