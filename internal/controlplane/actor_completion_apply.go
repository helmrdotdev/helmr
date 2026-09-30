package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (s *Server) completeActor(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.CompleteActorRequest, completion parsedActorCompletion) error {
	fence := workerExecutionFence(worker, completion.lease, request.Lease)
	err := s.inTx(ctx, func(work *txWork) error { return completeActorExecution(ctx, work.tx, fence, completion) })
	if err == nil {
		return nil
	}
	// Preserve a durable receipt across uncertain commit or concurrent completion.
	replayed, replayErr := actorExecutionReplayed(ctx, s.db, fence, completion.fingerprint)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, errStaleActorCompletion) {
		return replayErr
	}
	if replayErr != nil {
		return errors.Join(err, fmt.Errorf("check actor completion replay: %w", replayErr))
	}
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return workergroup.ErrStaleClaims
	}
	if errors.Is(err, secret.ErrDeliveryUnavailable) {
		return deterministicWorkerAdmission(err)
	}
	return err
}
