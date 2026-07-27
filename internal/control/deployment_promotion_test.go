package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestReconcileSchedulesInstallsOnlyScheduledTasks(t *testing.T) {
	target := promotionTestDeployment()
	scheduled := promotionTaskDefinition(
		t,
		target,
		"daily-report",
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"key":"scheduler"}}}`,
	)
	ordinary := promotionTaskDefinition(
		t,
		target,
		"on-demand",
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{scheduled, ordinary},
	}
	effectiveFrom := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)

	if err := reconcileSchedules(t.Context(), store, target, effectiveFrom); err != nil {
		t.Fatal(err)
	}
	if len(store.reconciled) != 1 {
		t.Fatalf("reconciled = %d, want 1", len(store.reconciled))
	}
	got := store.reconciled[0]
	if got.TaskDeclaredID != "daily-report" ||
		got.DeploymentID != target.ID ||
		got.DeploymentDefinitionID != scheduled.ID ||
		got.WorkspaceRefKey.String != "scheduler" ||
		got.WorkspaceID.Valid ||
		got.State != "pending_workspace" ||
		got.NextFireAt.Valid {
		t.Fatalf("reconciled Schedule = %+v", got)
	}
	if len(store.archived) != 1 ||
		len(store.archived[0].TaskDeclaredIds) != 1 ||
		store.archived[0].TaskDeclaredIds[0] != "daily-report" {
		t.Fatalf("archive filter = %+v", store.archived)
	}
}

func TestReconcileSchedulesPinsResolvedWorkspaceAndCursor(t *testing.T) {
	target := promotionTestDeployment()
	definition := promotionTaskDefinition(
		t,
		target,
		"heartbeat",
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"*/5 * * * *","timezone":"UTC","workspace":{"id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`,
	)
	workspaceID := uuid.Must(uuid.NewV7())
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{definition},
		workspaceID: pgvalue.UUID(workspaceID),
	}
	effectiveFrom := time.Date(2026, 7, 24, 3, 2, 0, 0, time.UTC)

	if err := reconcileSchedules(t.Context(), store, target, effectiveFrom); err != nil {
		t.Fatal(err)
	}
	got := store.reconciled[0]
	if got.State != "active" ||
		got.WorkspaceRefID != pgvalue.UUID(workspaceID) ||
		got.WorkspaceID != pgvalue.UUID(workspaceID) ||
		!got.NextFireAt.Valid ||
		!got.NextFireAt.Time.Equal(time.Date(2026, 7, 24, 3, 5, 0, 0, time.UTC)) {
		t.Fatalf("active Schedule = %+v", got)
	}
}

func TestReconcileSchedulesRejectsMissingIDWorkspaceBeforeMutation(t *testing.T) {
	target := promotionTestDeployment()
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{promotionTaskDefinition(
			t,
			target,
			"daily-report",
			`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`,
		)},
		workspaceErr: pgx.ErrNoRows,
	}

	err := reconcileSchedules(t.Context(), store, target, time.Now().UTC())
	if err == nil || len(store.reconciled) != 0 || len(store.archived) != 0 {
		t.Fatalf("reconcile error = %v, reconciled = %d, archived = %d", err, len(store.reconciled), len(store.archived))
	}
	var statusError apiError
	if !errors.As(err, &statusError) || statusError.kind != errBadRequest {
		t.Fatalf("error = %v, want bad request", err)
	}
}

func promotionTestDeployment() db.Deployment {
	return db.Deployment{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}
}

func promotionTaskDefinition(
	t *testing.T,
	target db.Deployment,
	declaredID string,
	raw string,
) db.DeploymentDefinition {
	t.Helper()
	canonical, digest, err := deployment.CanonicalManifestAndDigest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return db.DeploymentDefinition{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:   target.EnvironmentID,
		DeploymentID:    target.ID,
		Kind:            "task",
		DeclaredID:      declaredID,
		ManifestVersion: deployment.BuildPlanFormatVersion,
		Manifest:        canonical,
		ManifestDigest:  digest[:],
	}
}

type promotionScheduleStore struct {
	db.Querier
	definitions  []db.DeploymentDefinition
	workspaceID  pgtype.UUID
	workspaceErr error
	reconciled   []db.ReconcileScheduleParams
	archived     []db.ArchiveOmittedSchedulesParams
}

func (s *promotionScheduleStore) ListDeploymentDefinitionsForDeployment(
	context.Context,
	db.ListDeploymentDefinitionsForDeploymentParams,
) ([]db.DeploymentDefinition, error) {
	return s.definitions, nil
}

func (s *promotionScheduleStore) ResolveWorkspaceTarget(
	context.Context,
	db.ResolveWorkspaceTargetParams,
) (pgtype.UUID, error) {
	if s.workspaceErr != nil {
		return pgtype.UUID{}, s.workspaceErr
	}
	if !s.workspaceID.Valid {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return s.workspaceID, nil
}

func (s *promotionScheduleStore) ReconcileSchedule(
	_ context.Context,
	params db.ReconcileScheduleParams,
) error {
	s.reconciled = append(s.reconciled, params)
	return nil
}

func (s *promotionScheduleStore) ArchiveOmittedSchedules(
	_ context.Context,
	params db.ArchiveOmittedSchedulesParams,
) error {
	s.archived = append(s.archived, params)
	return nil
}

func TestScheduleResponseProjectsOnlyPublicAuthority(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	row := db.Schedule{
		PublicID:             "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		TaskDeclaredID:       "daily-report",
		WorkspaceRefKey:      pgvalue.Text("scheduler"),
		CronPattern:          "0 9 * * *",
		Timezone:             "UTC",
		CronSemanticsVersion: "robfig-cron-v3.0.1/standard-5-field",
		Generation:           4,
		State:                "errored",
		EffectiveFrom:        pgvalue.Timestamptz(now),
		NextFireAt:           pgvalue.Timestamptz(now.Add(time.Hour)),
		LastErrorCode:        pgvalue.Text("workspace-unavailable"),
		LastErrorMessage:     pgvalue.Text("Workspace is unavailable"),
		CreatedAt:            pgvalue.Timestamptz(now.Add(-time.Hour)),
		UpdatedAt:            pgvalue.Timestamptz(now),
	}
	response, err := scheduleResponse(row, pgtype.Text{})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != row.PublicID ||
		response.Task != row.TaskDeclaredID ||
		response.Workspace.Key != "scheduler" ||
		response.Status != "errored" ||
		response.LastError == nil ||
		response.LastError.Code != "workspace-unavailable" {
		t.Fatalf("response = %+v", response)
	}
}

func TestScheduleResponseUsesWorkspacePublicID(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	row := db.Schedule{
		PublicID:             "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		TaskDeclaredID:       "daily-report",
		WorkspaceRefID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CronPattern:          "0 9 * * *",
		Timezone:             "UTC",
		CronSemanticsVersion: "robfig-cron-v3.0.1/standard-5-field",
		Generation:           1,
		State:                "active",
		EffectiveFrom:        pgvalue.Timestamptz(now),
		NextFireAt:           pgvalue.Timestamptz(now.Add(time.Hour)),
		CreatedAt:            pgvalue.Timestamptz(now),
		UpdatedAt:            pgvalue.Timestamptz(now),
	}
	response, err := scheduleResponse(
		row,
		pgvalue.Text("wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Workspace.ID != "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("workspace ID = %q", response.Workspace.ID)
	}
}
