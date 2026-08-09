package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) adminListRegions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListRegions(r.Context())
	if err != nil {
		writeError(w, errors.New("list regions"))
		return
	}
	response := api.AdminRegionsResponse{Regions: make([]api.AdminRegion, 0, len(rows))}
	for _, row := range rows {
		response.Regions = append(response.Regions, adminRegion(row))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) adminGetRegion(w http.ResponseWriter, r *http.Request) {
	row, err := s.db.GetRegion(r.Context(), chi.URLParam(r, "regionID"))
	if isNoRows(err) {
		writeError(w, notFound(errors.New("region not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get region"))
		return
	}
	writeJSON(w, http.StatusOK, adminRegion(row))
}

func (s *Server) adminCreateRegion(w http.ResponseWriter, r *http.Request) {
	var request api.CreateAdminRegionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid region request JSON: %w", err)))
		return
	}
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.Location = strings.TrimSpace(request.Location)
	if err := region.ValidateID(request.ID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.DisplayName == "" {
		writeError(w, badRequest(errors.New("display_name is required")))
		return
	}
	created, err := s.db.CreateRegion(r.Context(), db.CreateRegionParams{
		ID: request.ID, DisplayName: request.DisplayName, Location: request.Location,
	})
	if isUniqueViolation(err) {
		writeError(w, conflict(errors.New("region identity is already in use")))
		return
	}
	if err != nil {
		writeError(w, errors.New("create region"))
		return
	}
	writeJSON(w, http.StatusCreated, adminRegion(created))
}

func (s *Server) adminUpdateRegion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "regionID")
	current, err := s.db.GetRegion(r.Context(), id)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("region not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get region"))
		return
	}
	var request api.UpdateAdminRegionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid region request JSON: %w", err)))
		return
	}
	displayName, location := current.DisplayName, current.Location
	if request.DisplayName != nil {
		displayName = strings.TrimSpace(*request.DisplayName)
	}
	if request.Location != nil {
		location = strings.TrimSpace(*request.Location)
	}
	if displayName == "" {
		writeError(w, badRequest(errors.New("display_name is required")))
		return
	}
	updated, err := s.db.UpdateRegionMetadata(r.Context(), db.UpdateRegionMetadataParams{
		ID: id, DisplayName: displayName, Location: location,
	})
	if err != nil {
		writeError(w, errors.New("update region"))
		return
	}
	writeJSON(w, http.StatusOK, adminRegion(updated))
}

func (s *Server) adminListWorkerGroups(w http.ResponseWriter, r *http.Request) {
	var regionID pgtype.Text
	if raw := r.URL.Query().Get("region_id"); raw != "" {
		if err := region.ValidateID(raw); err != nil {
			writeError(w, badRequest(err))
			return
		}
		regionID = pgtype.Text{String: raw, Valid: true}
	}
	rows, err := s.db.ListWorkerGroups(r.Context(), db.ListWorkerGroupsParams{RegionID: regionID, RowLimit: maxPageSize})
	if err != nil {
		writeError(w, errors.New("list worker groups"))
		return
	}
	response := api.AdminWorkerGroupsResponse{WorkerGroups: make([]api.AdminWorkerGroup, 0, len(rows))}
	for _, row := range rows {
		response.WorkerGroups = append(response.WorkerGroups, adminWorkerGroup(row))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) adminGetWorkerGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetWorkerGroup(r.Context(), groupID)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker group not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get worker group"))
		return
	}
	writeJSON(w, http.StatusOK, adminWorkerGroup(row))
}

func (s *Server) adminCreateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	var request api.CreateAdminWorkerGroupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker group request JSON: %w", err)))
		return
	}
	if err := region.ValidateID(request.RegionID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := workergroup.ValidateName(request.Name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	description := strings.TrimSpace(request.Description)
	if err := workergroup.ValidateRoles(request.AllowsRun, request.AllowsBuild); err != nil {
		writeError(w, badRequest(err))
		return
	}
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		writeError(w, errors.New("generate worker group token"))
		return
	}
	var created db.WorkerGroup
	err = s.inTx(r.Context(), func(work *txWork) error {
		if err := work.q.LockWorkerGroupCreationRegion(r.Context(), pglock.Key("helmr:worker-group-create:"+request.RegionID)); err != nil {
			return errors.New("lock worker group creation")
		}
		_, err := work.q.GetRegion(r.Context(), request.RegionID)
		if isNoRows(err) {
			return notFound(errors.New("region not found"))
		}
		if err != nil {
			return errors.New("get worker group region")
		}
		created, err = work.q.CreateWorkerGroup(r.Context(), db.CreateWorkerGroupParams{
			ID: uuid.Must(uuid.NewV7()).String(), TokenID: pgvalue.UUID(uuid.Must(uuid.NewV7())), TokenHash: token.Hash,
			RegionID: request.RegionID, Name: request.Name, Description: description,
			AllowsRun: request.AllowsRun, AllowsBuild: request.AllowsBuild,
		})
		if isUniqueViolation(err) {
			return conflict(errors.New("worker group conflicts with an existing active role or name"))
		}
		if err != nil {
			return errors.New("create worker group")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, api.CreateAdminWorkerGroupResponse{
		WorkerGroup: adminWorkerGroup(created), EnrollmentToken: token.Raw,
	})
}

func (s *Server) adminUpdateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.UpdateAdminWorkerGroupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker group request JSON: %w", err)))
		return
	}
	row, err := s.db.UpdateWorkerGroupDescription(r.Context(), db.UpdateWorkerGroupDescriptionParams{
		ID: groupID, Description: strings.TrimSpace(request.Description),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker group not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("update worker group"))
		return
	}
	writeJSON(w, http.StatusOK, adminWorkerGroup(row))
}

func (s *Server) adminPauseWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerGroup(w, r, workergroup.PauseGroup)
}

func (s *Server) adminActivateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerGroup(w, r, workergroup.ActivateGroup)
}

func (s *Server) adminDrainWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerGroup(w, r, workergroup.BeginGroupDrain)
}

func (s *Server) adminDisableWorkerGroup(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerGroup(w, r, workergroup.DisableGroup)
}

type groupTransition func(context.Context, workergroup.StateStore, string, int64) (workergroup.GroupStatus, error)

func (s *Server) adminTransitionWorkerGroup(w http.ResponseWriter, r *http.Request, transition groupTransition) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.WorkerGroupLifecycleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid lifecycle request JSON: %w", err)))
		return
	}
	if request.ExpectedClaimVersion <= 0 {
		writeError(w, badRequest(errors.New("expected_claim_version must be positive")))
		return
	}
	var status workergroup.GroupStatus
	err := s.inTx(r.Context(), func(work *txWork) error {
		if err := work.q.LockWorkerGroupMutation(r.Context(), workergroup.StateMutationLockKey(groupID)); err != nil {
			return errors.New("lock worker group lifecycle")
		}
		if _, err := work.q.GetWorkerGroupState(r.Context(), groupID); isNoRows(err) {
			return notFound(errors.New("worker group not found"))
		} else if err != nil {
			return errors.New("read worker group lifecycle")
		}
		var err error
		status, err = transition(r.Context(), work.q, groupID, request.ExpectedClaimVersion)
		if errors.Is(err, workergroup.ErrStateConflict) {
			return conflict(errors.New("worker group state or claim version changed"))
		}
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) adminRotateWorkerGroupToken(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		writeError(w, errors.New("generate worker group token"))
		return
	}
	if _, err := s.db.RotateWorkerGroupToken(r.Context(), db.RotateWorkerGroupTokenParams{
		WorkerGroupID: groupID, TokenHash: token.Hash,
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("worker group not found")))
		return
	} else if err != nil {
		writeError(w, errors.New("rotate worker group token"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, api.RotateWorkerGroupTokenResponse{EnrollmentToken: token.Raw})
}

func adminRegion(row db.Region) api.AdminRegion {
	return api.AdminRegion{
		ID: row.ID, DisplayName: row.DisplayName, Location: row.Location,
	}
}

func adminWorkerGroup(row db.WorkerGroup) api.AdminWorkerGroup {
	return api.AdminWorkerGroup{
		ID: row.ID, RegionID: row.RegionID, Name: row.Name, Description: row.Description,
		State: row.State, ClaimVersion: row.ClaimVersion, AllowsRun: row.AllowsRun, AllowsBuild: row.AllowsBuild,
	}
}

func adminGroupID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "groupID")
	parsed, err := ids.Parse(raw)
	if err != nil {
		writeError(w, badRequest(errors.New("worker group ID must be a canonical UUIDv7")))
		return "", false
	}
	return parsed.String(), true
}
