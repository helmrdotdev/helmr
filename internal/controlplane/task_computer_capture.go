package controlplane

import (
	"context"
	"errors"
	"uuid"

	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type parsedTaskComputerCapture struct {
	receipt workspace.FinalizationRequest
	disk    workerapi.CheckpointComputer
}

// Version metadata is independent of the producer's wire representation.
type workspaceVersionCapture struct{ root computer.GenerationRoot }

func (c parsedTaskComputerCapture) version() workspaceVersionCapture {
	return workspaceVersionCapture{root: c.disk.Root}
}

func (s *Server) verifyTaskComputerCapture(capture parsedTaskComputerCapture) (parsedTaskComputerCapture, error) {
	return capture, capture.disk.Root.Validate(capture.disk.LogicalBytes)
}

func requireFinalizationComputer(ctx context.Context, q db.Querier, authority runLeaseClaimAuthority, capture parsedTaskComputerCapture) error {
	if capture.disk.ComputerID != pgvalue.UUIDString(authority.workspace.ID) || capture.disk.LogicalBytes != authority.runtime.ReservedGuestEphemeralDiskBytes {
		return errStaleRunFinalization
	}
	rawRoot, err := json.Marshal(capture.disk.Root)
	if err != nil {
		return err
	}
	_, err = q.RequireRunFinalizationRoot(ctx, db.RequireRunFinalizationRootParams{RunLeaseID: authority.runLease.ID, OperationID: pgvalue.UUID(uuid.MustParse(capture.receipt.OperationID)), Root: rawRoot})
	if err != nil {
		return err
	}
	return requireCertifiedComputerRoot(ctx, q, authority, computerPublicationKey("finalization", authority.runLease.ID, pgvalue.UUID(uuid.MustParse(capture.receipt.OperationID))), capture.disk.Root)
}

// Check after all potentially blocking publication writes. The transaction keeps
// the locked authority unchanged; expiry still advances while a write is blocked.
func checkFinalizationPublicationDeadline(ctx context.Context, q db.Querier, authority runLeaseClaimAuthority) error {
	now, err := q.GetTaskCompletionTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid {
		return errors.New("database finalization time is unavailable")
	}
	return validateTaskCompletionDeadline(authority, now.Time)
}

func staleActorCompletionPublicationDeadline(ctx context.Context, q db.Querier, authority runLeaseClaimAuthority) error {
	err := checkFinalizationPublicationDeadline(ctx, q, authority)
	if errors.Is(err, errStaleTaskCompletion) {
		return errStaleActorCompletion
	}
	return err
}
