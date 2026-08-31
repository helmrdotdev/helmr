package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestReconcileSchedulesPinsPerFireWorkspaceAuthority(t *testing.T) {
	target := promotionTestDeployment()
	scheduled := promotionTaskDefinition(
		t,
		target,
		"daily-report",
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"reporting","secrets":[{"env":"REPORT_TOKEN","name":"REPORT_TOKEN"}]}}}`,
	)
	ordinary := promotionTaskDefinition(
		t,
		target,
		"on-demand",
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	sandbox := promotionSandboxDefinition(target, "reporting")
	secretID := pgvalue.UUID(uuid.NewV7())
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{scheduled, ordinary, sandbox},
		secrets: map[string]db.Secret{
			"REPORT_TOKEN": {ID: secretID},
		},
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
		got.State != "active" ||
		!got.NextFireAt.Valid ||
		!got.NextFireAt.Time.Equal(time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("reconciled Schedule = %+v", got)
	}
	if len(store.createdSecrets) != 1 ||
		store.createdSecrets[0].PlacementKind != "env" ||
		store.createdSecrets[0].PlacementTarget != "REPORT_TOKEN" ||
		store.createdSecrets[0].SecretID != secretID {
		t.Fatalf("Schedule Secret selection = %+v", store.createdSecrets)
	}
	if len(store.archived) != 1 ||
		len(store.archived[0].TaskDeclaredIds) != 1 ||
		store.archived[0].TaskDeclaredIds[0] != "daily-report" {
		t.Fatalf("archive filter = %+v", store.archived)
	}
}

func TestReconcileSchedulesRejectsMissingSandboxBeforeMutation(t *testing.T) {
	target := promotionTestDeployment()
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{promotionTaskDefinition(
			t,
			target,
			"daily-report",
			`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"missing","secrets":[]}}}`,
		)},
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

func TestReconcileSchedulesLocksSchedulesBeforeSecrets(t *testing.T) {
	target := promotionTestDeployment()
	sandbox := promotionSandboxDefinition(target, "runtime")
	store := &promotionScheduleStore{
		definitions: []db.DeploymentDefinition{
			promotionTaskDefinition(t, target, "z-task", `{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"runtime","secrets":[{"env":"Z_TOKEN","name":"Z_TOKEN"}]}}}`),
			promotionTaskDefinition(t, target, "a-task", `{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"runtime","secrets":[{"env":"A_TOKEN","name":"A_TOKEN"}]}}}`),
			sandbox,
		},
		secrets: map[string]db.Secret{
			"A_TOKEN": {ID: pgvalue.UUID(uuid.NewV7())},
			"Z_TOKEN": {ID: pgvalue.UUID(uuid.NewV7())},
		},
	}

	if err := reconcileSchedules(t.Context(), store, target, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"reconcile:a-task",
		"reconcile:z-task",
		"archive",
		"secrets",
	}
	if len(store.events) < len(wantPrefix) {
		t.Fatalf("events = %v, want prefix %v", store.events, wantPrefix)
	}
	for i, want := range wantPrefix {
		if store.events[i] != want {
			t.Fatalf("events = %v, want prefix %v", store.events, wantPrefix)
		}
	}
	if len(store.lockedNames) != 2 || store.lockedNames[0] != "A_TOKEN" || store.lockedNames[1] != "Z_TOKEN" {
		t.Fatalf("Secret lookup names = %v, want canonical complete set", store.lockedNames)
	}
}

func TestPromoteDeploymentRejectsLegacyReasonField(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"reason":"rollback"}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("deploymentID", uuid.NewV7().String())
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	recorder := httptest.NewRecorder()

	server.promoteDeployment(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "reason") {
		t.Fatalf("error = %s, want rejected reason field", recorder.Body.String())
	}
}

func TestScheduleResponseProjectsTimedDeclaration(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	scheduleID := uuid.NewV7()
	row := db.Schedule{
		ID:                   pgvalue.UUID(scheduleID),
		TaskDeclaredID:       "daily-report",
		CronPattern:          "0 9 * * *",
		Timezone:             "UTC",
		CronSemanticsVersion: "robfig-cron-v3.0.1/standard-5-field",
		Generation:           4,
		State:                "errored",
		EffectiveFrom:        pgvalue.Timestamptz(now),
		NextFireAt:           pgvalue.Timestamptz(now.Add(time.Hour)),
		LastFailure:          []byte(`{"code":"sandbox_authority_invalid","message":"Sandbox is unavailable","details":{}}`),
		CreatedAt:            pgvalue.Timestamptz(now.Add(-time.Hour)),
		UpdatedAt:            pgvalue.Timestamptz(now),
	}
	response, err := scheduleResponse(row)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != scheduleID.String() ||
		response.TaskID != row.TaskDeclaredID ||
		response.Status != api.ScheduleStatusErrored ||
		response.LastFailure == nil ||
		response.LastFailure.Code != "sandbox_authority_invalid" {
		t.Fatalf("response = %+v", response)
	}
}

func promotionTestDeployment() db.Deployment {
	return db.Deployment{
		ID:            pgvalue.UUID(uuid.NewV7()),
		OrgID:         pgvalue.UUID(uuid.NewV7()),
		ProjectID:     pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: pgvalue.UUID(uuid.NewV7()),
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
		ID:              pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:   target.EnvironmentID,
		DeploymentID:    target.ID,
		Kind:            "task",
		DeclaredID:      declaredID,
		ManifestVersion: deployment.DeploymentPlanFormatVersion,
		Manifest:        canonical,
		ManifestDigest:  digest[:],
	}
}

func promotionSandboxDefinition(target db.Deployment, declaredID string) db.DeploymentDefinition {
	return db.DeploymentDefinition{
		ID:            pgvalue.UUID(uuid.NewV7()),
		EnvironmentID: target.EnvironmentID,
		DeploymentID:  target.ID,
		Kind:          "sandbox",
		DeclaredID:    declaredID,
	}
}

type promotionScheduleStore struct {
	db.Querier
	definitions    []db.DeploymentDefinition
	secrets        map[string]db.Secret
	reconciled     []db.ReconcileScheduleParams
	createdSecrets []db.CreateScheduleSecretParams
	archived       []db.ArchiveOmittedSchedulesParams
	events         []string
	lockedNames    []string
}

func (s *promotionScheduleStore) ListDeploymentDefinitionsForDeployment(
	context.Context,
	db.ListDeploymentDefinitionsForDeploymentParams,
) ([]db.DeploymentDefinition, error) {
	return s.definitions, nil
}

func (s *promotionScheduleStore) ReconcileSchedule(
	_ context.Context,
	params db.ReconcileScheduleParams,
) (db.ReconcileScheduleRow, error) {
	s.reconciled = append(s.reconciled, params)
	s.events = append(s.events, "reconcile:"+params.TaskDeclaredID)
	return db.ReconcileScheduleRow{ID: params.ID, EnvironmentID: params.EnvironmentID}, nil
}

func (s *promotionScheduleStore) LockActiveSecretsByNameForWorkspaceCreate(
	_ context.Context,
	params db.LockActiveSecretsByNameForWorkspaceCreateParams,
) ([]db.Secret, error) {
	s.events = append(s.events, "secrets")
	s.lockedNames = append([]string(nil), params.Names...)
	secrets := make([]db.Secret, 0, len(params.Names))
	for _, name := range params.Names {
		secret, ok := s.secrets[name]
		if ok {
			secret.Name = name
			secrets = append(secrets, secret)
		}
	}
	return secrets, nil
}

func (s *promotionScheduleStore) DeleteScheduleSecrets(
	context.Context,
	db.DeleteScheduleSecretsParams,
) error {
	return nil
}

func (s *promotionScheduleStore) CreateScheduleSecret(
	_ context.Context,
	params db.CreateScheduleSecretParams,
) (db.ScheduleSecret, error) {
	s.createdSecrets = append(s.createdSecrets, params)
	return db.ScheduleSecret{}, nil
}

func (s *promotionScheduleStore) ArchiveOmittedSchedules(
	_ context.Context,
	params db.ArchiveOmittedSchedulesParams,
) error {
	s.archived = append(s.archived, params)
	s.events = append(s.events, "archive")
	return nil
}
