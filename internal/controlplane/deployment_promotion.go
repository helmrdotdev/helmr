package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	var request api.PromoteDeploymentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment promotion request: %w", err)))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(
		r,
		actor,
		request.ProjectID,
		request.EnvironmentID,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	principal, err := auth.ActorPrincipal(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if len(reason) > 1024 {
		writeError(w, badRequest(errors.New("promotion reason exceeds 1024 bytes")))
		return
	}
	effectiveFrom := time.Now().UTC()
	err = s.inTx(r.Context(), func(work *txWork) error {
		target, err := work.q.LockDeploymentPromotionTarget(
			r.Context(),
			db.LockDeploymentPromotionTargetParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				DeploymentID:  pgvalue.UUID(deploymentID),
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound(errors.New("deployment not found or is not deployable"))
		}
		if err != nil {
			return fmt.Errorf("lock deployment promotion target: %w", err)
		}
		if err := reconcileSchedules(
			r.Context(),
			work.q,
			target,
			effectiveFrom,
		); err != nil {
			return err
		}
		if _, err := work.q.PromoteDeployment(
			r.Context(),
			db.PromoteDeploymentParams{
				ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:               target.OrgID,
				ProjectID:           target.ProjectID,
				EnvironmentID:       target.EnvironmentID,
				DeploymentID:        target.ID,
				PromotedByPrincipal: principal,
				Reason:              reason,
			},
		); err != nil {
			return fmt.Errorf("promote deployment: %w", err)
		}
		if err := appendDeploymentLifecycleEvent(
			r.Context(),
			work.q,
			target.OrgID,
			target.ProjectID,
			target.EnvironmentID,
			target.ID,
			"deployment.promoted",
			"info",
			"control",
			"promoted",
			"Deployment promoted",
		); err != nil {
			return fmt.Errorf("record deployment promotion event: %w", err)
		}
		return nil
	})
	if err != nil {
		writeDeploymentError(w, s, err)
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	record, err := store.GetDeployment(r.Context(), db.GetDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(deploymentID),
	})
	if err != nil {
		writeError(w, fmt.Errorf("get promoted deployment: %w", err))
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, record)
	if err != nil {
		writeError(w, fmt.Errorf("get promoted deployment artifacts: %w", err))
		return
	}
	if err := populateDeploymentDeclarations(r.Context(), store, record, &response); err != nil {
		writeError(w, fmt.Errorf("get promoted deployment declarations: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func reconcileSchedules(
	ctx context.Context,
	store db.Querier,
	target db.Deployment,
	effectiveFrom time.Time,
) error {
	definitions, err := store.ListDeploymentDefinitionsForDeployment(
		ctx,
		db.ListDeploymentDefinitionsForDeploymentParams{
			EnvironmentID: target.EnvironmentID,
			DeploymentID:  target.ID,
			Kind:          pgvalue.Text(string(deployment.DefinitionKindTask)),
		},
	)
	if err != nil {
		return fmt.Errorf("list scheduled task declarations: %w", err)
	}
	scheduledIDs := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		manifest, err := deployment.ParseTaskManifest(
			definition.ManifestVersion,
			definition.Manifest,
			definition.ManifestDigest,
		)
		if err != nil {
			return fmt.Errorf("parse task %q for schedule reconciliation: %w", definition.DeclaredID, err)
		}
		if manifest.Schedule == nil {
			continue
		}
		scheduledIDs = append(scheduledIDs, definition.DeclaredID)
		if err := reconcileSchedule(
			ctx,
			store,
			target,
			definition,
			*manifest.Schedule,
			effectiveFrom,
		); err != nil {
			return err
		}
	}
	if err := store.ArchiveOmittedSchedules(
		ctx,
		db.ArchiveOmittedSchedulesParams{
			EffectiveFrom:   pgvalue.Timestamptz(effectiveFrom),
			EnvironmentID:   target.EnvironmentID,
			TaskDeclaredIds: scheduledIDs,
		},
	); err != nil {
		return fmt.Errorf("archive omitted schedules: %w", err)
	}
	return nil
}

func reconcileSchedule(
	ctx context.Context,
	store db.Querier,
	target db.Deployment,
	definition db.DeploymentDefinition,
	manifest deployment.ScheduleManifest,
	effectiveFrom time.Time,
) error {
	if err := schedule.ValidateCron(manifest.Cron); err != nil {
		return badRequest(fmt.Errorf("schedule %q cron: %w", definition.DeclaredID, err))
	}
	if err := schedule.ValidateTimezone(manifest.Timezone); err != nil {
		return badRequest(fmt.Errorf("schedule %q timezone: %w", definition.DeclaredID, err))
	}
	workspaceRefID, workspaceRefKey, workspaceID, state, err := resolveScheduleWorkspace(
		ctx,
		store,
		target,
		manifest.Workspace,
	)
	if err != nil {
		return fmt.Errorf("resolve schedule %q workspace: %w", definition.DeclaredID, err)
	}
	var nextFireAt pgtype.Timestamptz
	if state == "active" {
		next, err := schedule.NextCronTime(
			manifest.Cron,
			manifest.Timezone,
			effectiveFrom,
		)
		if err != nil {
			return badRequest(fmt.Errorf("schedule %q next fire: %w", definition.DeclaredID, err))
		}
		nextFireAt = pgvalue.Timestamptz(next)
	}
	if err := store.ReconcileSchedule(ctx, db.ReconcileScheduleParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:          target.EnvironmentID,
		TaskDeclaredID:         definition.DeclaredID,
		DeploymentDefinitionID: definition.ID,
		DeploymentID:           target.ID,
		WorkspaceRefID:         workspaceRefID,
		WorkspaceRefKey:        workspaceRefKey,
		WorkspaceID:            workspaceID,
		CronPattern:            manifest.Cron,
		Timezone:               manifest.Timezone,
		CronSemanticsVersion:   schedule.CronSemanticsVersion,
		State:                  state,
		EffectiveFrom:          pgvalue.Timestamptz(effectiveFrom),
		NextFireAt:             nextFireAt,
	}); err != nil {
		return fmt.Errorf("reconcile schedule %q: %w", definition.DeclaredID, err)
	}
	return nil
}

func resolveScheduleWorkspace(
	ctx context.Context,
	store db.Querier,
	target db.Deployment,
	address deployment.WorkspaceTarget,
) (pgtype.UUID, pgtype.Text, pgtype.UUID, string, error) {
	params := db.ResolveWorkspaceTargetParams{
		OrgID:         target.OrgID,
		ProjectID:     target.ProjectID,
		EnvironmentID: target.EnvironmentID,
	}
	if address.ID != nil {
		id, err := ids.Parse(*address.ID)
		if err != nil {
			return pgtype.UUID{}, pgtype.Text{}, pgtype.UUID{}, "", badRequest(err)
		}
		params.ID = pgvalue.UUID(id)
		workspaceID, err := store.ResolveWorkspaceTarget(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, pgtype.Text{}, pgtype.UUID{}, "", badRequest(
				fmt.Errorf("ID-addressed workspace %q does not exist", *address.ID),
			)
		}
		return workspaceID, pgtype.Text{}, workspaceID, "active", err
	}
	params.Key = pgvalue.Text(*address.Key)
	workspaceID, err := store.ResolveWorkspaceTarget(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, params.Key, pgtype.UUID{}, "pending_workspace", nil
	}
	if err != nil {
		return pgtype.UUID{}, pgtype.Text{}, pgtype.UUID{}, "", err
	}
	return pgtype.UUID{}, params.Key, workspaceID, "active", nil
}
