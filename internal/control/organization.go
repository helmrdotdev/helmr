package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	if actor.UserID == uuidNil {
		writeError(w, unauthorized(errors.New("session authentication is required")))
		return
	}
	var request api.CreateOrganizationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid organization request JSON: %w", err)))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var org db.Organization
	err = s.inTx(r.Context(), func(work *txWork) error {
		if s.selfHostedMode() {
			if !s.initialSetupTokenMatches(request.SetupToken) {
				return forbidden(errors.New("invalid setup token"))
			}
			if err := work.q.LockOrganizationsForSelfHostedSetup(r.Context()); err != nil {
				return errors.New("create organization")
			}
			organizationCount, err := work.q.CountOrganizations(r.Context())
			if err != nil {
				return errors.New("create organization")
			}
			if organizationCount > 0 {
				return conflict(errors.New("organization already exists"))
			}
		}
		var err error
		org, err = work.q.CreateOrganization(r.Context(), db.CreateOrganizationParams{
			ID:   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Name: name,
			Slug: slug,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return badRequest(errors.New("organization slug is already in use"))
			}
			return errors.New("create organization")
		}
		if _, err := work.q.EnsureOrgCell(r.Context(), db.EnsureOrgCellParams{
			OrgID:  org.ID,
			CellID: s.cellID,
			Role:   db.OrgCellRoleHome,
			State:  db.OrgCellStateActive,
		}); err != nil {
			return errors.New("create organization cell")
		}
		if _, err := work.q.EnsureOrgMember(r.Context(), db.EnsureOrgMemberParams{
			OrgID:       org.ID,
			UserID:      pgvalue.UUID(actor.UserID),
			Role:        db.OrgMemberRoleOwner,
			DisplayName: pgtype.Text{},
		}); err != nil {
			return errors.New("create organization owner")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, organizationResponse(org))
}

func (s *Server) initialSetupTokenMatches(token string) bool {
	expected := strings.TrimSpace(s.setupToken)
	provided := strings.TrimSpace(token)
	if expected == "" || provided == "" {
		return false
	}
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

type workerBootstrapTokenStore interface {
	EnsureDefaultWorkerGroup(context.Context, string) (db.WorkerGroup, error)
	UpsertWorkerBootstrapToken(context.Context, db.UpsertWorkerBootstrapTokenParams) (db.WorkerBootstrapToken, error)
}

func (s *Server) ensureWorkerBootstrapToken(ctx context.Context, queries workerBootstrapTokenStore) error {
	if s.workerRegisterToken == "" {
		return nil
	}
	tokenHash, err := auth.HashToken(s.authSecret, s.workerRegisterToken)
	if err != nil {
		return err
	}
	workerGroup, err := queries.EnsureDefaultWorkerGroup(ctx, s.cellID)
	if err != nil {
		return fmt.Errorf("get default worker group: %w", err)
	}
	_, err = queries.UpsertWorkerBootstrapToken(ctx, db.UpsertWorkerBootstrapTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CellID:        s.cellID,
		TokenHash:     tokenHash,
		WorkerGroupID: workerGroup.ID,
	})
	return err
}

func (s *Server) selfHostedMode() bool {
	return s.deploymentMode != deploymentModeManagedCloud
}

func organizationResponse(org db.Organization) api.OrganizationSummary {
	return api.OrganizationSummary{
		ID:        pgvalue.MustUUIDValue(org.ID).String(),
		Slug:      org.Slug,
		Name:      org.Name,
		CreatedAt: pgvalue.Time(org.CreatedAt),
	}
}
