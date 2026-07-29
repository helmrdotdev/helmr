package control

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/jackc/pgx/v5/pgtype"
)

type parsedRunLeaseReceipt struct {
	leaseID                uuid.UUID
	runID                  uuid.UUID
	workerInstanceID       uuid.UUID
	runtimeInstanceID      uuid.UUID
	networkSlotID          uuid.UUID
	workspaceID            uuid.UUID
	workspaceMountID       uuid.UUID
	workspaceLeaseID       uuid.UUID
	baseWorkspaceVersionID uuid.UUID
}

func parseRunLeaseReceipt(receipt api.WorkerRunLeaseReceipt) (parsedRunLeaseReceipt, error) {
	parseID := func(name, value string) (uuid.UUID, error) {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return uuid.Nil, fmt.Errorf("%s must be a canonical UUID", name)
		}
		return parsed, nil
	}
	var parsed parsedRunLeaseReceipt
	ids := []struct {
		name  string
		value string
		dest  *uuid.UUID
	}{
		{"lease.id", receipt.ID, &parsed.leaseID},
		{"lease.run_id", receipt.RunID, &parsed.runID},
		{"lease.worker_instance_id", receipt.WorkerInstanceID, &parsed.workerInstanceID},
		{"lease.runtime_instance_id", receipt.RuntimeInstanceID, &parsed.runtimeInstanceID},
		{"lease.network_slot_id", receipt.NetworkSlotID, &parsed.networkSlotID},
		{"lease.workspace_id", receipt.WorkspaceID, &parsed.workspaceID},
		{"lease.workspace_mount_id", receipt.WorkspaceMountID, &parsed.workspaceMountID},
		{"lease.workspace_lease_id", receipt.WorkspaceLeaseID, &parsed.workspaceLeaseID},
		{"lease.base_workspace_version_id", receipt.BaseWorkspaceVersionID, &parsed.baseWorkspaceVersionID},
	}
	for _, id := range ids {
		value, err := parseID(id.name, id.value)
		if err != nil {
			return parsedRunLeaseReceipt{}, err
		}
		*id.dest = value
	}
	if receipt.AttemptNumber <= 0 ||
		receipt.LeaseSequence <= 0 ||
		receipt.WorkerEpoch <= 0 ||
		receipt.NetworkSlotGeneration <= 0 ||
		receipt.OwnershipGeneration <= 0 ||
		receipt.WriterGeneration <= 0 ||
		receipt.MountFencingGeneration <= 0 ||
		receipt.RequestedCPUMillis <= 0 ||
		receipt.RequestedMemoryBytes <= 0 ||
		receipt.RequestedGuestEphemeralDiskBytes < 0 ||
		receipt.RequestedExecutionSlots <= 0 ||
		receipt.MaxActiveDurationMs <= 0 ||
		receipt.ActiveElapsedMs < 0 {
		return parsedRunLeaseReceipt{}, errors.New("lease numeric fields are invalid")
	}
	stringsToValidate := []struct {
		name  string
		value string
	}{
		{"lease.worker_group_id", receipt.WorkerGroupID},
		{"lease.worker_protocol_version", receipt.WorkerProtocolVersion},
		{"lease.runtime_identity_id", receipt.RuntimeIdentityID},
	}
	for _, field := range stringsToValidate {
		if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
			return parsedRunLeaseReceipt{}, fmt.Errorf("%s is invalid", field.name)
		}
	}
	if receipt.StartDeadlineAt.IsZero() ||
		receipt.ExpiresAt.IsZero() ||
		receipt.StartDeadlineAt.After(receipt.ExpiresAt) {
		return parsedRunLeaseReceipt{}, errors.New("lease timestamps are invalid")
	}
	if receipt.WorkerProtocolVersion != api.CurrentWorkerProtocolVersion {
		return parsedRunLeaseReceipt{}, errors.New("lease worker protocol version is unsupported")
	}
	return parsed, nil
}

func equalRunLeaseReceipt(left, right api.WorkerRunLeaseReceipt) bool {
	leftDeadline, rightDeadline := left.StartDeadlineAt, right.StartDeadlineAt
	leftExpiry, rightExpiry := left.ExpiresAt, right.ExpiresAt
	left.StartDeadlineAt, right.StartDeadlineAt = time.Time{}, time.Time{}
	left.ExpiresAt, right.ExpiresAt = time.Time{}, time.Time{}
	return left == right &&
		leftDeadline.Equal(rightDeadline) &&
		leftExpiry.Equal(rightExpiry)
}

// equalCurrentOrPreviousRunLeaseReceipt accepts the one-generation expiry
// overlap retained by Run Lease renewal. All immutable and fencing fields must
// still match exactly.
func equalCurrentOrPreviousRunLeaseReceipt(
	current api.WorkerRunLeaseReceipt,
	candidate api.WorkerRunLeaseReceipt,
	previousExpiry pgtype.Timestamptz,
) bool {
	if equalRunLeaseReceipt(current, candidate) {
		return true
	}
	if !previousExpiry.Valid {
		return false
	}
	previous := current
	previous.ExpiresAt = previousExpiry.Time
	return equalRunLeaseReceipt(previous, candidate)
}
