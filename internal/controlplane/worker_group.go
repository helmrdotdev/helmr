package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// The organization worker group API calls the same workergroup operations as
// the operator's admin routes, as an organization operator: every group it
// names must be one the caller's organization owns, and any other group reads
// as missing.

func (s *Server) mountWorkerGroupRoutes(r chi.Router) {
	read := s.sessionPermission(auth.PermissionWorkerGroupsRead)
	manage := s.sessionPermission(auth.PermissionWorkerGroupsManage)
	r.With(read).Get("/worker-groups", s.listWorkerGroups)
	r.With(manage).Post("/worker-groups", s.createWorkerGroup)
	r.With(read).Get("/worker-groups/{groupID}", s.getWorkerGroup)
	r.With(manage).Post("/worker-groups/{groupID}/pause", s.pauseWorkerGroup)
	r.With(manage).Post("/worker-groups/{groupID}/activate", s.activateWorkerGroup)
	r.With(manage).Post("/worker-groups/{groupID}/drain", s.drainWorkerGroup)
	r.With(manage).Post("/worker-groups/{groupID}/disable", s.disableWorkerGroup)
	r.With(manage).Post("/worker-groups/{groupID}/token/rotate", s.rotateWorkerGroupToken)
	r.With(manage).Post("/worker-groups/{groupID}/host-credentials/revoke", s.revokeWorkerGroupHostCredentials)
	r.With(read).Get("/worker-groups/{groupID}/hosts", s.listWorkerGroupHosts)
	r.With(manage).Post("/worker-groups/{groupID}/hosts/{hostID}/fence", s.fenceWorkerHost)
	r.With(s.sessionPermission(auth.PermissionPlacementsRead)).Get("/projects/{project}/environments/{environment}/placements", s.listPlacements)
	r.With(s.sessionPermission(auth.PermissionPlacementsUpdate)).Put("/projects/{project}/environments/{environment}/placements/{placement}", s.setPlacement)
	r.With(s.sessionPermission(auth.PermissionPlacementsUpdate)).Delete("/projects/{project}/environments/{environment}/placements/{placement}", s.removePlacement)
}

// workerGroupOperator acts for the caller's organization, rechecking the
// member's permission in each operation's transaction.
func (s *Server) workerGroupOperator(r *http.Request, permission auth.Permission) workergroup.Operator {
	m := s.managingMember(principalFromContext(r.Context()))
	return workergroup.OrganizationOperator(m.OrgID, func(ctx context.Context, tx pgx.Tx) (audit.Evidence, error) {
		if err := m.Reauthorize(ctx, tx, org.ScopedPermission{Permission: permission, Scope: auth.Scope{OrgID: m.OrgID}}); err != nil {
			return audit.Evidence{}, err
		}
		return m.Actor, nil
	})
}

func (s *Server) listWorkerGroups(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	rows, err := workergroup.ListOrganizationGroups(r.Context(), s.db, principal.OrgID, r.URL.Query().Get("region_id"), maxPageSize)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	response := api.WorkerGroupsResponse{WorkerGroups: make([]api.WorkerGroup, 0, len(rows))}
	for _, row := range rows {
		response.WorkerGroups = append(response.WorkerGroups, organizationWorkerGroup(row))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getWorkerGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	row, err := workergroup.GetOrganizationGroup(r.Context(), s.db, principalFromContext(r.Context()).OrgID, groupID)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, organizationWorkerGroup(row))
}

// Creating a group issues its enrollment token, so it needs recent proof.
func (s *Server) createWorkerGroup(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireRecentProof(principalFromContext(r.Context())); err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	var request api.CreateWorkerGroupRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid worker group request JSON: %w", err))
		return
	}
	created, err := workergroup.CreateGroup(r.Context(), s.tx, s.auditPolicy, s.workerGroupOperator(r, auth.PermissionWorkerGroupsManage), workergroup.GroupInput{
		RegionID: request.RegionID, Name: request.Name, Description: request.Description,
	})
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, api.CreateWorkerGroupResponse{
		WorkerGroup: organizationWorkerGroup(created.Group), EnrollmentToken: created.EnrollmentToken,
	})
}

func (s *Server) pauseWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.transitionWorkerGroup(w, r, workergroup.PauseGroup)
}

func (s *Server) activateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.transitionWorkerGroup(w, r, workergroup.ActivateGroup)
}

func (s *Server) drainWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.transitionWorkerGroup(w, r, workergroup.BeginGroupDrain)
}

func (s *Server) disableWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.transitionWorkerGroup(w, r, workergroup.DisableGroup)
}

func (s *Server) transitionWorkerGroup(w http.ResponseWriter, r *http.Request, transition groupTransition) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.WorkerGroupLifecycleRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid lifecycle request JSON: %w", err))
		return
	}
	status, err := transition(r.Context(), s.tx, s.auditPolicy, s.workerGroupOperator(r, auth.PermissionWorkerGroupsManage), groupID, request.ExpectedClaimVersion)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// Rotating issues a new enrollment token, so it needs recent proof.
func (s *Server) rotateWorkerGroupToken(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireRecentProof(principalFromContext(r.Context())); err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	token, err := workergroup.RotateGroupToken(r.Context(), s.tx, s.auditPolicy, s.workerGroupOperator(r, auth.PermissionWorkerGroupsManage), groupID)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, api.RotateWorkerGroupTokenResponse{EnrollmentToken: token})
}

func (s *Server) revokeWorkerGroupHostCredentials(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	fenced, err := workergroup.RevokeGroupHostCredentials(r.Context(), s.tx, s.auditPolicy, s.workerGroupOperator(r, auth.PermissionWorkerGroupsManage), groupID)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	response := api.RevokeWorkerGroupHostCredentialsResponse{FencedWorkerHostIDs: make([]string, 0, len(fenced))}
	for _, hostID := range fenced {
		response.FencedWorkerHostIDs = append(response.FencedWorkerHostIDs, hostID.String())
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listWorkerGroupHosts(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	orgID := principalFromContext(r.Context()).OrgID
	if _, err := workergroup.GetOrganizationGroup(r.Context(), s.db, orgID, groupID); err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	hosts, err := workergroup.ListHosts(r.Context(), s.db, workergroup.HostFilter{OwnerOrgID: orgID, GroupID: groupID, Limit: maxPageSize})
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

// fenceWorkerHost records that the host no longer holds the group's work and
// fences its credentials. It does not stop the machine itself.
func (s *Server) fenceWorkerHost(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	hostID, err := ids.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker host ID must be a canonical UUIDv7")))
		return
	}
	var request api.FenceWorkerHostRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid worker host fence request JSON: %w", err))
		return
	}
	host, err := workergroup.GetOrganizationHost(r.Context(), s.db, principalFromContext(r.Context()).OrgID, hostID)
	if err == nil && host.WorkerGroupID != groupID.String() {
		err = workergroup.ErrHostNotFound
	}
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	status, err := workergroup.MarkHostLost(r.Context(), s.tx, s.auditPolicy, s.workerGroupOperator(r, auth.PermissionWorkerGroupsManage), groupID, host.ResourceID, hostID, request.ExpectedClaimVersion)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) listPlacements(w http.ResponseWriter, r *http.Request) {
	_, environmentID, ok := s.placementEnvironment(w, r)
	if !ok {
		return
	}
	placements, err := workergroup.ListPlacements(r.Context(), s.db, environmentID)
	if err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	response := api.PlacementsResponse{Placements: make([]api.Placement, 0, len(placements))}
	for _, placement := range placements {
		response.Placements = append(response.Placements, apiPlacement(placement))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) setPlacement(w http.ResponseWriter, r *http.Request) {
	scope, environmentID, ok := s.placementEnvironment(w, r)
	if !ok {
		return
	}
	var request api.SetPlacementRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid placement request JSON: %w", err))
		return
	}
	placement := workergroup.Placement{Name: chi.URLParam(r, "placement")}
	if request.WorkerGroupID != "" {
		groupID, err := ids.Parse(request.WorkerGroupID)
		if err != nil {
			writeError(w, badRequest(errors.New("worker_group_id must be a canonical UUIDv7")))
			return
		}
		placement.WorkerGroupID = &groupID
	}
	if err := workergroup.SetPlacement(r.Context(), s.tx, s.placementEditor(r, scope), environmentID, placement); err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiPlacement(placement))
}

func (s *Server) removePlacement(w http.ResponseWriter, r *http.Request) {
	scope, environmentID, ok := s.placementEnvironment(w, r)
	if !ok {
		return
	}
	if _, err := workergroup.RemovePlacement(r.Context(), s.tx, s.placementEditor(r, scope), environmentID, chi.URLParam(r, "placement")); err != nil {
		s.writeOrganizationWorkerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// placementEditor rechecks placements.update in the environment scope, and
// worker_groups.use when the edit names an organization group, in the edit's
// transaction.
func (s *Server) placementEditor(r *http.Request, scope auth.Scope) workergroup.PlacementEditor {
	m := s.managingMember(principalFromContext(r.Context()))
	return workergroup.PlacementEditor{Policy: s.auditPolicy, Authorize: func(ctx context.Context, tx pgx.Tx, group bool) (audit.Evidence, error) {
		required := []org.ScopedPermission{{Permission: auth.PermissionPlacementsUpdate, Scope: scope}}
		if group {
			required = append(required, org.ScopedPermission{Permission: auth.PermissionWorkerGroupsUse, Scope: auth.Scope{OrgID: m.OrgID}})
		}
		if err := m.Reauthorize(ctx, tx, required...); err != nil {
			return audit.Evidence{}, err
		}
		return m.Actor, nil
	}}
}

func (s *Server) placementEnvironment(w http.ResponseWriter, r *http.Request) (auth.Scope, uuid.UUID, bool) {
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principalFromContext(r.Context()))
	if err != nil {
		s.writeOperationError(w, r, err)
		return auth.Scope{}, uuid.Nil(), false
	}
	return scope, pgvalue.MustUUIDValue(environmentID), true
}

// writeOrganizationWorkerError writes the errors of the organization worker
// group API, including the membership errors its reauthorization returns.
func (s *Server) writeOrganizationWorkerError(w http.ResponseWriter, r *http.Request, err error) {
	var unbound workergroup.UnboundPlacementsError
	switch {
	case errors.Is(err, org.ErrMemberManagementRequired):
		s.writeOperationError(w, r, orgError(err))
	case errors.As(err, &unbound):
		s.writeOperationError(w, r, conflict(codedError{code: "placements_unbound", message: unbound.Error()}))
	case errors.Is(err, workergroup.ErrPlacementGroup):
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_placement_worker_group", message: err.Error()}))
	case errors.Is(err, workergroup.ErrEnvironmentNotFound):
		s.writeOperationError(w, r, notFound(err))
	default:
		s.writeWorkerGroupError(w, r, err)
	}
}

func organizationWorkerGroup(row db.WorkerGroup) api.WorkerGroup {
	return api.WorkerGroup{
		ID: pgvalue.UUIDString(row.ID), RegionID: row.RegionID, Name: row.Name, Description: row.Description,
		Status: row.Status, ClaimVersion: row.ClaimVersion,
	}
}

func apiPlacement(placement workergroup.Placement) api.Placement {
	result := api.Placement{Name: placement.Name}
	if placement.WorkerGroupID != nil {
		result.WorkerGroupID = placement.WorkerGroupID.String()
	}
	return result
}
