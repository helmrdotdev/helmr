package controlplane

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func projectSecretDeliveries(materials []secret.DeliveryMaterial) ([]workerapi.SecretDelivery, error) {
	ordered := append([]secret.DeliveryMaterial(nil), materials...)
	slices.SortFunc(ordered, func(left, right secret.DeliveryMaterial) int {
		if compared := strings.Compare(left.PlacementKind, right.PlacementKind); compared != 0 {
			return compared
		}
		return strings.Compare(left.PlacementTarget, right.PlacementTarget)
	})
	deliveries := make([]workerapi.SecretDelivery, 0, len(ordered))
	for index, material := range ordered {
		if strings.TrimSpace(material.PlacementTarget) == "" {
			return nil, errors.New("secret placement target is required")
		}
		if index > 0 &&
			ordered[index-1].PlacementKind == material.PlacementKind &&
			ordered[index-1].PlacementTarget == material.PlacementTarget {
			return nil, errors.New("secret placement is duplicated")
		}
		delivery := workerapi.SecretDelivery{Value: append([]byte(nil), material.Value...)}
		switch material.PlacementKind {
		case "env":
			delivery.Env = &workerapi.SecretEnv{Name: material.PlacementTarget}
		case "file":
			delivery.File = &workerapi.SecretFile{Path: material.PlacementTarget}
		default:
			return nil, fmt.Errorf("secret placement kind %q is unsupported", material.PlacementKind)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

type runLeaseProjectionAuthority struct {
	run      db.Run
	attempt  db.RunAttempt
	instance db.ComputerInstance
	runLease db.RunLease
	computer db.LockRunLeaseClaimComputerRow
}

func projectRunLeaseAssignment(authority runLeaseProjectionAuthority) (workerapi.RunLeaseAssignment, error) {
	lease := authority.runLease
	id, err := requiredClaimUUIDString("run lease ID", lease.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	runID, err := requiredClaimUUIDString("run ID", lease.RunID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	workerID, err := requiredClaimUUIDString("worker instance ID", lease.WorkerHostID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	instanceID, err := requiredClaimUUIDString("runtime instance ID", lease.ComputerInstanceID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	computerID, err := requiredClaimUUIDString("computer ID", authority.computer.ID)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	baseComputerDiskVersionID, err := requiredClaimUUIDString(
		"base computer version ID",
		authority.attempt.BaseComputerDiskVersionID,
	)
	if err != nil {
		return workerapi.RunLeaseAssignment{}, err
	}
	if lease.RunID != authority.run.ID ||
		lease.AttemptNumber != authority.attempt.Number ||
		authority.attempt.RunID != authority.run.ID ||
		authority.attempt.ComputerID != authority.computer.ID ||
		lease.ComputerID != authority.computer.ID ||
		lease.ComputerInstanceID != authority.instance.ID ||
		authority.instance.ComputerID != lease.ComputerID ||
		authority.instance.WorkerGroupID != lease.WorkerGroupID ||
		authority.instance.WorkerHostID != lease.WorkerHostID ||
		authority.instance.WorkerEpoch != lease.WorkerEpoch ||
		authority.instance.EnvironmentID != authority.run.EnvironmentID ||
		authority.instance.WriterGeneration != authority.computer.WriterGeneration {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment authority is inconsistent")
	}
	if lease.AttemptNumber <= 0 ||
		lease.LeaseSequence <= 0 ||
		lease.WorkerEpoch <= 0 ||
		authority.instance.WriterGeneration <= 0 ||
		lease.RequestedCPUMillis <= 0 ||
		lease.RequestedMemoryBytes <= 0 ||
		lease.RequestedGuestEphemeralDiskBytes <= 0 ||
		lease.RequestedExecutionSlots <= 0 ||
		authority.run.MaxActiveDurationMs <= 0 ||
		authority.run.ActiveElapsedMs < 0 ||
		!lease.StartDeadlineAt.Valid ||
		!lease.ExpiresAt.Valid ||
		lease.StartDeadlineAt.Time.After(lease.ExpiresAt.Time) {
		return workerapi.RunLeaseAssignment{}, errors.New("run lease assignment fields are invalid")
	}
	if !lease.WorkerGroupID.Valid {
		return workerapi.RunLeaseAssignment{}, errors.New("worker group ID is required")
	}
	for name, value := range map[string]string{
		"VM platform ID": authority.instance.VMPlatformID,
	} {
		if strings.TrimSpace(value) == "" {
			return workerapi.RunLeaseAssignment{}, fmt.Errorf("%s is required", name)
		}
	}
	return workerapi.RunLeaseAssignment{
		ID:                               id,
		RunID:                            runID,
		AttemptNumber:                    lease.AttemptNumber,
		LeaseSequence:                    lease.LeaseSequence,
		WorkerGroupID:                    pgvalue.UUIDString(lease.WorkerGroupID),
		WorkerHostID:                     workerID,
		WorkerEpoch:                      lease.WorkerEpoch,
		ComputerInstanceID:               instanceID,
		VMPlatformID:                     authority.instance.VMPlatformID,
		ComputerID:                       computerID,
		BaseComputerDiskVersionID:        baseComputerDiskVersionID,
		WriterGeneration:                 authority.instance.WriterGeneration,
		RequestedCPUMillis:               lease.RequestedCPUMillis,
		RequestedMemoryBytes:             lease.RequestedMemoryBytes,
		RequestedGuestEphemeralDiskBytes: lease.RequestedGuestEphemeralDiskBytes,
		RequestedExecutionSlots:          lease.RequestedExecutionSlots,
		MaxActiveDurationMs:              authority.run.MaxActiveDurationMs,
		ActiveElapsedMs:                  authority.run.ActiveElapsedMs,
		Trace: api.TraceContext{
			TraceID:     lease.TraceID.String,
			SpanID:      lease.SpanID.String,
			Traceparent: lease.Traceparent.String,
		},
		StartDeadlineAt: lease.StartDeadlineAt.Time,
		ExpiresAt:       lease.ExpiresAt.Time,
	}, nil
}

func projectComputerAttachment(
	authority runLeaseProjectionAuthority,
	writeCapability string,
	resetAuthority db.GetComputerDiskVersionAuthorityRow,
) (workerapi.ComputerAttachment, error) {
	if _, err := projectRunLeaseAssignment(authority); err != nil {
		return workerapi.ComputerAttachment{}, err
	}
	if strings.TrimSpace(writeCapability) == "" {
		return workerapi.ComputerAttachment{}, errors.New("computer write capability is required")
	}
	if resetAuthority.VersionID != authority.attempt.BaseComputerDiskVersionID {
		return workerapi.ComputerAttachment{}, errors.New("computer reset version does not match Attempt base")
	}
	id, err := requiredClaimUUIDString("computer reset version ID", resetAuthority.VersionID)
	if err != nil {
		return workerapi.ComputerAttachment{}, err
	}
	return workerapi.ComputerAttachment{WriteCapability: writeCapability, Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: id}}, nil
}
