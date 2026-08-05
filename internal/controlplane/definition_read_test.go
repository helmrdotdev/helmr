package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveDefinitionDeploymentDistinguishesSelectionStates(t *testing.T) {
	t.Parallel()
	orgID := pgvalue.UUID(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"))
	projectID := pgvalue.UUID(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"))
	environmentID := pgvalue.UUID(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"))
	deploymentID := pgvalue.UUID(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"))

	tests := []struct {
		name     string
		selector pgtype.UUID
		store    definitionReadStore
		wantCode string
		wantKind errKind
	}{
		{
			name:     "no current deployment",
			store:    definitionReadStore{currentErr: pgx.ErrNoRows},
			wantCode: "no_current_deployment", wantKind: errNotFound,
		},
		{
			name:     "exact deployment outside scope",
			selector: deploymentID,
			store:    definitionReadStore{exactErr: pgx.ErrNoRows},
			wantCode: "deployment_not_found", wantKind: errNotFound,
		},
		{
			name:     "exact deployment not materialized",
			selector: deploymentID,
			store:    definitionReadStore{exact: db.Deployment{ID: deploymentID, Status: "building"}},
			wantCode: "deployment_not_materialized", wantKind: errConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{db: &test.store}
			_, err := s.resolveDefinitionDeployment(
				context.Background(), test.selector, orgID, projectID, environmentID,
			)
			if errorStatus(err) != statusForKind(test.wantKind) {
				t.Fatalf("errorStatus() = %d, want %d: %v", errorStatus(err), statusForKind(test.wantKind), err)
			}
			var coder errorCoder
			if !errors.As(err, &coder) || coder.ErrorCode() != test.wantCode {
				t.Fatalf("ErrorCode() = %q, want %q", coder.ErrorCode(), test.wantCode)
			}
		})
	}

	materialized := db.Deployment{
		ID: deploymentID, Status: "deployed",
		ProgramArtifactID:  pgvalue.UUID(uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35")),
		ProgramIndexDigest: []byte{1}, BuildRuntimeDigest: []byte{2},
	}
	s := &Server{db: &definitionReadStore{current: materialized}}
	got, err := s.resolveDefinitionDeployment(
		context.Background(), pgtype.UUID{}, orgID, projectID, environmentID,
	)
	if err != nil || got != deploymentID {
		t.Fatalf("resolveDefinitionDeployment() = (%v, %v), want (%v, nil)", got, err, deploymentID)
	}
}

type definitionReadStore struct {
	db.Querier
	current    db.Deployment
	currentErr error
	exact      db.Deployment
	exactErr   error
}

func (s *definitionReadStore) GetCurrentDeploymentForRoute(
	context.Context,
	db.GetCurrentDeploymentForRouteParams,
) (db.Deployment, error) {
	return s.current, s.currentErr
}

func (s *definitionReadStore) GetDeployment(
	context.Context,
	db.GetDeploymentParams,
) (db.Deployment, error) {
	return s.exact, s.exactErr
}

func statusForKind(kind errKind) int {
	return errorStatus(apiError{kind: kind, err: errors.New("test")})
}
