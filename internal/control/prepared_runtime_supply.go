package control

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) reconcilePreparedRuntimeSupplyAsync(ctx context.Context, reason string) {
	if s.preparedRuntimeSupply == nil {
		return
	}
	go func() {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preparedRuntimeSupplyReconcileTimeout)
		defer cancel()
		if err := s.preparedRuntimeSupply.Reconcile(reconcileCtx); err != nil && s.log != nil {
			s.log.Warn("prepared runtime supply reconcile failed", "reason", reason, "error", err)
		}
	}()
}

func (s *Server) reconcilePreparedRuntimeSupplyForSandboxAsync(ctx context.Context, deploymentSandboxID pgtype.UUID, reason string) {
	if s.preparedRuntimeSupply == nil || !deploymentSandboxID.Valid {
		return
	}
	go func() {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preparedRuntimeSupplyReconcileTimeout)
		defer cancel()
		if err := s.preparedRuntimeSupply.ReconcileDeploymentSandbox(reconcileCtx, deploymentSandboxID); err != nil && s.log != nil {
			s.log.Warn(
				"prepared runtime supply sandbox reconcile failed",
				"reason", reason,
				"deployment_sandbox_id", deploymentSandboxID,
				"error", err,
			)
		}
	}()
}
