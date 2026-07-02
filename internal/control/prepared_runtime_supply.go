package control

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type preparedRuntimeSupplyWorkflow struct {
	log     *slog.Logger
	supply  PreparedRuntimeSupplyReconciler
	timeout time.Duration
}

func (s *Server) reconcilePreparedRuntimeSupply(ctx context.Context, reason string) {
	s.preparedRuntimeSupplyWorkflow().Reconcile(ctx, reason)
}

func (s *Server) reconcilePreparedRuntimeSupplyAsync(ctx context.Context, reason string) {
	s.preparedRuntimeSupplyWorkflow().ReconcileAsync(ctx, reason)
}

func (s *Server) reconcilePreparedRuntimeSupplyForSandbox(ctx context.Context, deploymentSandboxID pgtype.UUID, reason string) {
	s.preparedRuntimeSupplyWorkflow().ReconcileDeploymentSandbox(ctx, deploymentSandboxID, reason)
}

func (s *Server) reconcilePreparedRuntimeSupplyForSandboxAsync(ctx context.Context, deploymentSandboxID pgtype.UUID, reason string) {
	s.preparedRuntimeSupplyWorkflow().ReconcileDeploymentSandboxAsync(ctx, deploymentSandboxID, reason)
}

func (s *Server) preparedRuntimeSupplyWorkflow() preparedRuntimeSupplyWorkflow {
	return preparedRuntimeSupplyWorkflow{
		log:     s.log,
		supply:  s.preparedRuntimeSupply,
		timeout: preparedRuntimeSupplyReconcileTimeout,
	}
}

func (w preparedRuntimeSupplyWorkflow) Reconcile(ctx context.Context, reason string) {
	if w.supply == nil {
		return
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.timeout)
	defer cancel()
	if err := w.supply.Reconcile(reconcileCtx); err != nil {
		w.warn("prepared runtime supply reconcile failed", "reason", reason, "error", err)
	}
}

func (w preparedRuntimeSupplyWorkflow) ReconcileAsync(ctx context.Context, reason string) {
	if w.supply == nil {
		return
	}
	go w.Reconcile(ctx, reason)
}

func (w preparedRuntimeSupplyWorkflow) ReconcileDeploymentSandbox(ctx context.Context, deploymentSandboxID pgtype.UUID, reason string) {
	if w.supply == nil || !deploymentSandboxID.Valid {
		return
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.timeout)
	defer cancel()
	if err := w.supply.ReconcileDeploymentSandbox(reconcileCtx, deploymentSandboxID); err != nil {
		w.warn(
			"prepared runtime supply sandbox reconcile failed",
			"reason", reason,
			"deployment_sandbox_id", deploymentSandboxID,
			"error", err,
		)
	}
}

func (w preparedRuntimeSupplyWorkflow) ReconcileDeploymentSandboxAsync(ctx context.Context, deploymentSandboxID pgtype.UUID, reason string) {
	if w.supply == nil || !deploymentSandboxID.Valid {
		return
	}
	go w.ReconcileDeploymentSandbox(ctx, deploymentSandboxID, reason)
}

func (w preparedRuntimeSupplyWorkflow) warn(message string, attrs ...any) {
	if w.log == nil {
		return
	}
	w.log.Warn(message, attrs...)
}
