package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	if s.tx == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("organization storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	if actor.UserID == uuidNil {
		writeError(w, http.StatusUnauthorized, errors.New("session authentication is required"))
		return
	}
	var request api.CreateOrganizationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid organization request JSON: %w", err))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create organization"))
		return
	}
	defer tx.Rollback(r.Context())
	if s.selfHostedMode() {
		if !s.initialSetupTokenMatches(request.SetupToken) {
			writeError(w, http.StatusForbidden, errors.New("invalid setup token"))
			return
		}
		if _, err := tx.Exec(r.Context(), `LOCK TABLE organizations IN EXCLUSIVE MODE`); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("create organization"))
			return
		}
		var organizationCount int64
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM organizations`).Scan(&organizationCount); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("create organization"))
			return
		}
		if organizationCount > 0 {
			writeError(w, http.StatusConflict, errors.New("organization already exists"))
			return
		}
	}
	queries := db.New(tx)
	org, err := queries.CreateOrganization(r.Context(), db.CreateOrganizationParams{
		ID:   ids.ToPG(ids.New()),
		Name: name,
		Slug: slug,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("organization slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create organization"))
		return
	}
	if _, err := queries.EnsureOrgMember(r.Context(), db.EnsureOrgMemberParams{
		OrgID:       org.ID,
		UserID:      ids.ToPG(actor.UserID),
		Role:        db.OrgMemberRoleOwner,
		DisplayName: pgtype.Text{},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create organization owner"))
		return
	}
	if err := s.ensureOrganizationWorkerPool(r.Context(), queries, org.ID); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create organization worker pool"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create organization"))
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

func (s *Server) ensureOrganizationWorkerPool(ctx context.Context, queries *db.Queries, orgID pgtype.UUID) error {
	pool, err := queries.EnsureDefaultWorkerPool(ctx, ids.ToPG(ids.New()))
	if err != nil {
		return err
	}
	if _, err := queries.UpsertOrgWorkerPool(ctx, db.UpsertOrgWorkerPoolParams{
		OrgID:        orgID,
		WorkerPoolID: pool.ID,
		IsDefault:    true,
	}); err != nil {
		return err
	}
	return s.ensureOrganizationWorkerRegistrationToken(ctx, queries, pool.ID)
}

func (s *Server) ensureOrganizationWorkerRegistrationToken(ctx context.Context, queries *db.Queries, workerPoolID pgtype.UUID) error {
	if s.workerRegisterToken == "" {
		return nil
	}
	tokenHash, err := auth.HashToken(s.authSecret, s.workerRegisterToken)
	if err != nil {
		return err
	}
	_, err = queries.UpsertWorkerRegistrationToken(ctx, db.UpsertWorkerRegistrationTokenParams{
		ID:           ids.ToPG(ids.New()),
		WorkerPoolID: workerPoolID,
		TokenHash:    tokenHash,
	})
	return err
}

func (s *Server) selfHostedMode() bool {
	return s.deploymentMode != deploymentModeManagedCloud
}

func organizationResponse(org db.Organization) api.OrganizationSummary {
	return api.OrganizationSummary{
		ID:        ids.MustFromPG(org.ID).String(),
		Slug:      org.Slug,
		Name:      org.Name,
		CreatedAt: pgTime(org.CreatedAt),
	}
}
