package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.UserID == uuid.Nil() {
		writeError(w, unauthorized(errors.New("session authentication is required")))
		return
	}
	var request api.CreateOrganizationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid organization request JSON: %w", err))
		return
	}
	initialSetup := s.selfHostedMode()
	if initialSetup && !s.initialSetupTokenMatches(request.SetupToken) {
		writeError(w, forbidden(errors.New("invalid setup token")))
		return
	}
	created, err := org.CreateOrganization(r.Context(), s.tx, org.OrganizationInput{
		Slug:         request.Slug,
		Name:         request.Name,
		OwnerUserID:  principal.UserID,
		InitialSetup: initialSetup,
	})
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusCreated, organizationResponse(created))
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.db.ListRegions(r.Context())
	if err != nil {
		writeError(w, errors.New("list regions"))
		return
	}
	response := api.ListRegionsResponse{Regions: make([]api.RegionSummary, 0, len(regions))}
	for _, region := range regions {
		response.Regions = append(response.Regions, api.RegionSummary{
			ID: region.ID, DisplayName: region.DisplayName, Location: region.Location,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) initialSetupTokenMatches(token string) bool {
	expected := s.setupToken
	provided := token
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

func organizationResponse(organization db.Organization) api.OrganizationSummary {
	return api.OrganizationSummary{
		ID:        pgvalue.MustUUIDValue(organization.ID).String(),
		Slug:      organization.Slug,
		Name:      organization.Name,
		CreatedAt: pgvalue.Time(organization.CreatedAt),
	}
}

// orgError maps errors of the org owner to HTTP errors.
func orgError(err error) error {
	var input org.InputError
	switch {
	case errors.As(err, &input),
		errors.Is(err, org.ErrOrganizationSlugInUse),
		errors.Is(err, org.ErrProjectSlugInUse),
		errors.Is(err, org.ErrEnvironmentSlugInUse),
		errors.Is(err, org.ErrNoRegion),
		errors.Is(err, org.ErrDefaultRegionNotFound):
		return badRequest(err)
	case errors.Is(err, org.ErrMemberManagementRequired),
		errors.Is(err, org.ErrOwnerRoleRequired),
		errors.Is(err, org.ErrLastActiveOwner),
		errors.Is(err, org.ErrSelfMemberManagement),
		errors.Is(err, org.ErrSelfMemberRemoval):
		return forbidden(err)
	case errors.Is(err, org.ErrProjectNotFound),
		errors.Is(err, org.ErrEnvironmentNotFound),
		errors.Is(err, org.ErrMemberNotFound),
		errors.Is(err, org.ErrInvitationNotFound):
		return notFound(err)
	case errors.Is(err, org.ErrOrganizationExists),
		errors.Is(err, org.ErrMemberRoleChanged),
		errors.Is(err, org.ErrInvitationPending),
		errors.Is(err, org.ErrInvitationActiveMember):
		return conflict(err)
	default:
		return err
	}
}
