package telemetry

import (
	"time"

	"uuid"
)

type EventRecord struct {
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	SubjectKind    string     `json:"subject_kind"`
	SubjectID      uuid.UUID  `json:"subject_id"`
	EventKind      string     `json:"event_kind"`
	Seq            uint64     `json:"seq"`
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	DeploymentID   *uuid.UUID `json:"deployment_id,omitempty"`
	RunLeaseID     *uuid.UUID `json:"run_lease_id,omitempty"`
	AttemptNumber  *int32     `json:"attempt_number,omitempty"`
	TraceID        string     `json:"trace_id"`
	SpanID         string     `json:"span_id"`
	ParentSpanID   string     `json:"parent_span_id"`
	Traceparent    string     `json:"traceparent"`
	Category       string     `json:"category"`
	Severity       string     `json:"severity"`
	Source         string     `json:"source"`
	Message        string     `json:"message"`
	Body           string     `json:"body"`
	IdempotencyKey string     `json:"idempotency_key"`
	RetentionClass string     `json:"retention_class"`
	RedactionClass string     `json:"redaction_class"`
	ObservedAt     time.Time  `json:"observed_at"`
}

type RunLogRecord struct {
	OrgID          uuid.UUID `json:"org_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	EnvironmentID  uuid.UUID `json:"environment_id"`
	RunID          uuid.UUID `json:"run_id"`
	RunLeaseID     uuid.UUID `json:"run_lease_id"`
	AttemptNumber  int32     `json:"attempt_number"`
	StreamName     string    `json:"stream_name"`
	Seq            uint64    `json:"seq"`
	ObservedSeq    uint64    `json:"observed_seq"`
	Content        string    `json:"content"`
	SizeBytes      uint64    `json:"size_bytes"`
	IdempotencyKey string    `json:"idempotency_key"`
	RetentionClass string    `json:"retention_class"`
	RedactionClass string    `json:"redaction_class"`
	Source         string    `json:"source"`
	ObservedAt     time.Time `json:"observed_at"`
}
