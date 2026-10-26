package audit

import (
	"errors"
	"slices"
	"time"
	"uuid"
)

// FailureDetails never carries an error message, request body or credential.
type FailureDetails struct {
	Operation string `json:"operation,omitempty"`
	Reason    string `json:"reason"`
}

func (FailureDetails) auditDetails() {}
func validReason(reason string) bool {
	return slices.Contains([]string{"authentication_invalid", "permission_denied", "installation_authority_required", "invalid_request", "state_conflict", "stale_authority", "operation_failed", "worker_lost", "cancelled", "expired", "delivery_failed", "not_found", "capture_failed"}, reason)
}

type LifecycleDetails struct {
	Sequence   int64     `json:"sequence,omitempty"`
	Generation int64     `json:"generation,omitempty"`
	RequestID  uuid.UUID `json:"request_id,omitzero"`
}

func (LifecycleDetails) auditDetails() {}

type SecretExposureDetails struct {
	ExposureID           uuid.UUID `json:"exposure_id"`
	VersionID            uuid.UUID `json:"version_id"`
	RevocationGeneration int64     `json:"revocation_generation"`
	CommandID            uuid.UUID `json:"command_id,omitzero"`
	ImageBuildID         uuid.UUID `json:"image_build_id,omitzero"`
}

func (SecretExposureDetails) auditDetails() {}

type MCPDetails struct {
	Tool string `json:"tool"`
}

func (MCPDetails) auditDetails() {}

type ComputerRetentionDetails struct {
	Before *int64 `json:"before_ms"`
	After  *int64 `json:"after_ms"`
}

func (ComputerRetentionDetails) auditDetails() {}

type ComputerControlDetails struct {
	Reason    string    `json:"reason,omitempty"`
	Kind      string    `json:"kind"`
	RequestID uuid.UUID `json:"request_id"`
}

func (ComputerControlDetails) auditDetails() {}

type SessionControlDetails struct {
	RequestID uuid.UUID `json:"request_id"`
	HoldID    uuid.UUID `json:"hold_id,omitzero"`
}

func (SessionControlDetails) auditDetails() {}

type AuthenticationDetails struct {
	DeviceCodeID            uuid.UUID `json:"device_code_id,omitzero"`
	Method                  string    `json:"method"`
	Provider                string    `json:"provider,omitempty"`
	Subject                 string    `json:"subject,omitempty"`
	AuthorizingCredentialID uuid.UUID `json:"authorizing_credential_id,omitzero"`
}

func (AuthenticationDetails) auditDetails() {}

type SearchDetails struct {
	ReadModel              string      `json:"read_model,omitempty"`
	ActorKind              string      `json:"actor_kind,omitempty"`
	TargetKind             string      `json:"target_kind,omitempty"`
	TargetUserID           uuid.UUID   `json:"target_user_id,omitzero"`
	SlackTeamID            string      `json:"slack_team_id,omitempty"`
	SlackUserID            string      `json:"slack_user_id,omitempty"`
	RequestedEnvironmentID uuid.UUID   `json:"requested_environment_id,omitzero"`
	From                   time.Time   `json:"from"`
	To                     time.Time   `json:"to"`
	ResultCount            int         `json:"result_count"`
	EnvironmentIDs         []uuid.UUID `json:"environment_ids,omitzero"`
	Actions                []string    `json:"actions,omitempty"`
	OperationID            uuid.UUID   `json:"operation_id,omitzero"`
	EventID                uuid.UUID   `json:"event_id,omitzero"`
	ActorID                uuid.UUID   `json:"actor_id,omitzero"`
	TargetID               uuid.UUID   `json:"target_id,omitzero"`
	Outcome                string      `json:"outcome,omitempty"`
}

func (SearchDetails) auditDetails() {}

type RetentionObservedDetails struct {
	RetentionSeconds int64 `json:"retention_seconds"`
}

func (RetentionObservedDetails) auditDetails() {}

type PurgeDetails struct {
	Cutoff    time.Time `json:"cutoff"`
	Count     int64     `json:"count"`
	MinExpiry time.Time `json:"min_expiry"`
	MaxExpiry time.Time `json:"max_expiry"`
}

func (PurgeDetails) auditDetails() {}

type RestoreDetails struct {
	BackupReference string    `json:"backup_reference"`
	UnknownFrom     time.Time `json:"unknown_from"`
	UnknownTo       time.Time `json:"unknown_to"`
}

func (RestoreDetails) auditDetails() {}

func marshalEventDetails(action, outcome string, details Details) ([]byte, error) {
	if _, ok := Lookup(action); !ok {
		return nil, errors.New("unknown audit action")
	}
	if action == "computer.control_settled" {
		d, ok := details.(ComputerControlDetails)
		if !ok || (outcome == "failed" && !validReason(d.Reason)) || (outcome == "succeeded" && d.Reason != "") {
			return nil, errors.New("invalid Computer control outcome")
		}
		return marshalDetails(action, details)
	}
	if outcome == "denied" || outcome == "failed" || outcome == "unknown" {
		failure, ok := details.(FailureDetails)
		if !ok || !validReason(failure.Reason) || len(failure.Operation) > 512 {
			return nil, errors.New("safe audit failure reason is required")
		}
		return boundedJSON(failure, 16384)
	}
	return marshalDetails(action, details)
}

// RegionDetails identifies the installation's string-keyed Region without
// pretending that it has a UUID identity.
type RegionDetails struct {
	RegionID string `json:"region_id"`
}

func (RegionDetails) auditDetails() {}

// TemplateRefreshDetails identifies the build selected for this accepted request.
type TemplateRefreshDetails struct {
	ImageBuildID uuid.UUID `json:"image_build_id"`
}

func (TemplateRefreshDetails) auditDetails() {}

type AuthorityChangedDetails struct {
	HoldID     uuid.UUID `json:"hold_id"`
	Generation int64     `json:"generation,omitempty"`
}

func (AuthorityChangedDetails) auditDetails() {}

type CheckpointDetails struct {
	CheckpointID uuid.UUID `json:"checkpoint_id"`
	Generation   int64     `json:"generation,omitempty"`
}

func (CheckpointDetails) auditDetails() {}
