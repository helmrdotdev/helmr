package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) adminListRegions(w http.ResponseWriter, r *http.Request) {
	rows, err := region.List(r.Context(), s.db)
	if err != nil {
		s.writeRegionError(w, err)
		return
	}
	response := api.AdminRegionsResponse{Regions: make([]api.AdminRegion, 0, len(rows))}
	for _, row := range rows {
		response.Regions = append(response.Regions, adminRegion(row))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) adminGetRegion(w http.ResponseWriter, r *http.Request) {
	row, err := region.Get(r.Context(), s.db, chi.URLParam(r, "regionID"))
	if err != nil {
		s.writeRegionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminRegion(row))
}

func (s *Server) adminCreateRegion(w http.ResponseWriter, r *http.Request) {
	var request api.CreateAdminRegionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid region request JSON: %w", err))
		return
	}
	created, err := region.Create(r.Context(), s.db, region.Details{
		ID: request.ID, DisplayName: request.DisplayName, Location: request.Location,
	})
	if err != nil {
		s.writeRegionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, adminRegion(created))
}

func (s *Server) adminUpdateRegion(w http.ResponseWriter, r *http.Request) {
	var request api.UpdateAdminRegionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid region request JSON: %w", err))
		return
	}
	updated, err := region.Update(r.Context(), s.db, chi.URLParam(r, "regionID"), region.Patch{
		DisplayName: request.DisplayName, Location: request.Location,
	})
	if err != nil {
		s.writeRegionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminRegion(updated))
}

func (s *Server) adminListWorkerGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := workergroup.ListGroups(r.Context(), s.db, r.URL.Query().Get("region_id"), maxPageSize)
	if err != nil {
		s.writeWorkerGroupError(w, err)
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
	row, err := workergroup.GetGroup(r.Context(), s.db, groupID)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminWorkerGroup(row))
}

func (s *Server) adminCreateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	var request api.CreateAdminWorkerGroupRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker group request JSON: %w", err))
		return
	}
	created, err := workergroup.CreateGroup(r.Context(), s.tx, workergroup.GroupInput{
		RegionID: request.RegionID, Name: request.Name, Description: request.Description,
	})
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, api.CreateAdminWorkerGroupResponse{
		WorkerGroup: adminWorkerGroup(created.Group), EnrollmentToken: created.EnrollmentToken,
	})
}

func (s *Server) adminUpdateWorkerGroup(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.UpdateAdminWorkerGroupRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker group request JSON: %w", err))
		return
	}
	row, err := workergroup.UpdateGroupDescription(r.Context(), s.db, groupID, request.Description)
	if err != nil {
		s.writeWorkerGroupError(w, err)
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

type groupTransition func(context.Context, db.TxBeginner, uuid.UUID, int64) (workergroup.GroupStatus, error)

func (s *Server) adminTransitionWorkerGroup(w http.ResponseWriter, r *http.Request, transition groupTransition) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.WorkerGroupLifecycleRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid lifecycle request JSON: %w", err))
		return
	}
	status, err := transition(r.Context(), s.tx, groupID, request.ExpectedClaimVersion)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) adminRotateWorkerGroupToken(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	token, err := workergroup.RotateGroupToken(r.Context(), s.db, groupID)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, api.RotateWorkerGroupTokenResponse{EnrollmentToken: token})
}

func (s *Server) adminListWorkerPools(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	group, pools, err := workergroup.ListPools(r.Context(), s.db, groupID)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	response := api.AdminWorkerPoolsResponse{WorkerPools: make([]api.AdminWorkerPool, 0, len(pools))}
	for _, pool := range pools {
		response.WorkerPools = append(response.WorkerPools, adminWorkerPool(pool, group))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) adminCreateWorkerPool(w http.ResponseWriter, r *http.Request) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return
	}
	var request api.CreateAdminWorkerPoolRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker pool request JSON: %w", err))
		return
	}
	group, created, err := workergroup.CreatePool(r.Context(), s.tx, groupID, request.Name, request.ExpectedGroupClaimVersion)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, adminWorkerPool(created, group))
}

func (s *Server) adminSwitchWorkerPoolPrimary(w http.ResponseWriter, r *http.Request) {
	groupID, poolID, ok := adminWorkerPoolIDs(w, r)
	if !ok {
		return
	}
	var request api.SwitchAdminWorkerPoolPrimaryRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker pool primary request JSON: %w", err))
		return
	}
	selection, err := workergroup.SelectPrimaryPool(r.Context(), s.tx, groupID, poolID, request.ExpectedGroupClaimVersion)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SwitchAdminWorkerPoolPrimaryResponse{
		WorkerGroup: adminWorkerGroup(selection.Group), WorkerPool: adminWorkerPool(selection.Pool, selection.Group),
	})
}

func (s *Server) adminDrainWorkerPool(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerPool(w, r, workergroup.DrainPool)
}

func (s *Server) adminDisableWorkerPool(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionWorkerPool(w, r, workergroup.DisablePool)
}

type poolTransition func(context.Context, db.TxBeginner, uuid.UUID, uuid.UUID, int64) (db.WorkerGroup, db.WorkerPool, error)

func (s *Server) adminTransitionWorkerPool(w http.ResponseWriter, r *http.Request, transition poolTransition) {
	groupID, poolID, ok := adminWorkerPoolIDs(w, r)
	if !ok {
		return
	}
	var request api.WorkerPoolLifecycleRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker pool lifecycle request JSON: %w", err))
		return
	}
	group, pool, err := transition(r.Context(), s.tx, groupID, poolID, request.ExpectedPoolClaimVersion)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adminWorkerPool(pool, group))
}

func adminRegion(row db.Region) api.AdminRegion {
	return api.AdminRegion{
		ID: row.ID, DisplayName: row.DisplayName, Location: row.Location,
	}
}

func adminWorkerGroup(row db.WorkerGroup) api.AdminWorkerGroup {
	return api.AdminWorkerGroup{
		ID: pgvalue.UUIDString(row.ID), RegionID: row.RegionID, Name: row.Name, Description: row.Description,
		Status: row.Status, ClaimVersion: row.ClaimVersion,
		PrimaryPoolID: adminOptionalUUID(row.PrimaryPoolID),
	}
}

func adminWorkerPool(pool db.WorkerPool, group db.WorkerGroup) api.AdminWorkerPool {
	return api.AdminWorkerPool{
		ID: uuid.UUID(pool.ID.Bytes).String(), WorkerGroupID: pgvalue.UUIDString(pool.WorkerGroupID),
		Name: pool.Name, Status: pool.Status, ClaimVersion: pool.ClaimVersion,
		Primary: group.PrimaryPoolID.Valid && group.PrimaryPoolID.Bytes == pool.ID.Bytes,
	}
}

func adminOptionalUUID(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	return uuid.UUID(value.Bytes).String()
}

func adminGroupID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "groupID")
	parsed, err := ids.Parse(raw)
	if err != nil {
		writeError(w, badRequest(errors.New("worker group ID must be a canonical UUIDv7")))
		return uuid.Nil(), false
	}
	return parsed, true
}

func adminWorkerPoolIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	groupID, ok := adminGroupID(w, r)
	if !ok {
		return uuid.Nil(), uuid.Nil(), false
	}
	poolID, err := ids.Parse(chi.URLParam(r, "poolID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker pool ID must be a canonical UUIDv7")))
		return uuid.Nil(), uuid.Nil(), false
	}
	return groupID, poolID, true
}
