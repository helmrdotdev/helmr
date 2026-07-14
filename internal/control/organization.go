package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
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
		var publicID string
		org, err = createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.Organization, value: &publicID}}, func() (db.Organization, error) {
			return work.q.CreateOrganization(r.Context(), db.CreateOrganizationParams{
				ID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID: publicID,
				Name:     name,
				Slug:     slug,
			})
		})
		if err != nil {
			if isUniqueViolation(err) {
				return badRequest(errors.New("organization slug is already in use"))
			}
			return errors.New("create organization")
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

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("region storage is not configured")))
		return
	}
	regions, err := s.db.ListRegions(r.Context())
	if err != nil {
		writeError(w, errors.New("list regions"))
		return
	}
	response := api.ListRegionsResponse{Regions: make([]api.RegionSummary, 0, len(regions))}
	for _, region := range regions {
		response.Regions = append(response.Regions, api.RegionSummary{
			ID:             region.ID,
			Provider:       region.Provider,
			ProviderRegion: region.ProviderRegion,
			DisplayName:    region.DisplayName,
			State:          string(region.State),
			Visibility:     string(region.Visibility),
			Location:       region.Location,
			StaticIPs:      region.StaticIps,
		})
	}
	writeJSON(w, http.StatusOK, response)
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
