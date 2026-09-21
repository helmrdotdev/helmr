package controlplane

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
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
type workspaceVersionCapture struct {
	artifact      cas.Descriptor
	contentDigest string
	sizeBytes     int64
	entryCount    int32
}

func (c parsedTaskComputerCapture) version() workspaceVersionCapture {
	a := c.disk.Artifact
	return workspaceVersionCapture{artifact: cas.Descriptor{Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType}, contentDigest: a.Digest, sizeBytes: c.disk.LogicalBytes}
}

func (c parsedWorkspaceTreeCapture) version() workspaceVersionCapture {
	a := c.artifact
	return workspaceVersionCapture{artifact: cas.Descriptor{Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType}, contentDigest: c.tree.Digest, sizeBytes: c.tree.SizeBytes, entryCount: int32(c.tree.EntryCount)}
}

func (s *Server) verifyTaskComputerCapture(ctx context.Context, capture parsedTaskComputerCapture) (parsedTaskComputerCapture, error) {
	if s.cas == nil {
		return capture, errors.New("Computer CAS is not configured")
	}
	a := capture.disk.Artifact
	object, err := s.cas.Stat(ctx, a.Digest)
	if err != nil {
		return capture, fmt.Errorf("stat finalization Computer disk: %w", err)
	}
	if object.Digest != a.Digest || object.SizeBytes != a.SizeBytes || object.MediaType != a.MediaType {
		return capture, errors.New("finalization Computer disk does not match storage")
	}
	return capture, nil
}

func requireFinalizationComputer(ctx context.Context, q db.Querier, authority runLeaseClaimAuthority, capture parsedTaskComputerCapture) error {
	if capture.disk.ComputerID != pgvalue.UUIDString(authority.workspace.ID) || capture.disk.LogicalBytes != authority.runtime.ReservedGuestEphemeralDiskBytes {
		return errStaleRunFinalization
	}
	_, err := q.RequireRunFinalizationObject(ctx, db.RequireRunFinalizationObjectParams{RunLeaseID: authority.runLease.ID, OperationID: pgvalue.UUID(uuid.MustParse(capture.receipt.OperationID)), Digest: capture.disk.Artifact.Digest, SizeBytes: capture.disk.Artifact.SizeBytes, MediaType: capture.disk.Artifact.MediaType, LogicalBytes: capture.disk.LogicalBytes})
	return err
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
