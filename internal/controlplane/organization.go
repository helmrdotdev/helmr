package controlplane

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auth"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/identity"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.UserID == uuid.Nil() {
		s.writeOperationError(w, r, unauthorized(errors.New("session authentication is required")))
		return
	}
	var request api.CreateOrganizationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid organization request JSON: %w", err))
		return
	}
	count, err := s.db.CountOrganizations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	initialSetup := count == 0
	if !initialSetup && (s.selfHostedMode() || s.organizationCreationAdminOnly) {
		writeError(w, forbidden(errors.New("organization creation is disabled")))
		return
	}
	var operator auth.Principal
	if initialSetup {
		operator, _, err = identity.AuthenticateOperator(r.Context(), s.tx, s.identity, operatorToken(r))
		if err != nil {
			writeError(w, forbidden(errors.New("operator authentication is required for first-owner setup")))
			return
		}
	}
	created, err := identity.CreateOrganization(r.Context(), s.tx, org.OrganizationInput{AuditPolicy: s.auditPolicy,
		Actor:        audit.FromPrincipal(principal),
		Slug:         request.Slug,
		Name:         request.Name,
		OwnerUserID:  principal.UserID,
		InitialSetup: initialSetup,
	}, principal, operator)
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	writeJSON(w, http.StatusCreated, organizationResponse(created))
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.db.ListRegions(r.Context())
	if err != nil {
		s.writeOperationError(w, r, errors.New("list regions"))
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
		errors.Is(err, org.ErrLastActiveOwner):
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
