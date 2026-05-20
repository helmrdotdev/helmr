package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
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
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create organization"))
		return
	}
	writeJSON(w, http.StatusCreated, organizationResponse(org))
}

func organizationResponse(org db.Organization) api.OrganizationSummary {
	return api.OrganizationSummary{
		ID:        ids.MustFromPG(org.ID).String(),
		Slug:      org.Slug,
		Name:      org.Name,
		CreatedAt: pgTime(org.CreatedAt),
	}
}
