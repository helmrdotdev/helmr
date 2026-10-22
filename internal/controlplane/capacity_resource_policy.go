package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func capacityPoolIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	group, ok := capacityWorkerGroupID(w, r)
	if !ok {
		return group, uuid.Nil(), false
	}
	pool, err := ids.Parse(chi.URLParam(r, "workerPoolID"))
	if err != nil {
		writeError(w, badRequest(errors.New("worker_pool_id must be a canonical UUIDv7")))
		return group, uuid.Nil(), false
	}
	return group, pool, true
}

func (s *Server) capacityGetResourcePolicy(w http.ResponseWriter, r *http.Request) {
	group, pool, ok := capacityPoolIDs(w, r)
	if !ok {
		return
	}
	policy, err := workergroup.GetResourcePolicy(r.Context(), s.db, group, pool)
	if err != nil {
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) capacityPutResourcePolicy(w http.ResponseWriter, r *http.Request) {
	group, pool, ok := capacityPoolIDs(w, r)
	if !ok {
		return
	}
	// Pointers distinguish a required zero limit from an omitted or null field.
	var body struct {
		ExpectedRevision *int64 `json:"expected_revision"`
		Optional         *struct {
			CPUMillis        *int64 `json:"cpu_millis"`
			MemoryBytes      *int64 `json:"memory_bytes"`
			DiskBytes        *int64 `json:"disk_bytes"`
			VMSlots          *int32 `json:"vm_slots"`
			MaxTasks         *int32 `json:"max_tasks"`
			IOBytesPerSecond *int64 `json:"io_bytes_per_second"`
		} `json:"optional"`
		LocalHeadroomSlots *int32 `json:"local_headroom_slots"`
	}
	if err := decodeRequestJSON(r, &body); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if body.ExpectedRevision == nil || body.Optional == nil || body.LocalHeadroomSlots == nil || body.Optional.CPUMillis == nil || body.Optional.MemoryBytes == nil || body.Optional.DiskBytes == nil || body.Optional.VMSlots == nil || body.Optional.MaxTasks == nil || body.Optional.IOBytesPerSecond == nil {
		writeError(w, badRequest(codedError{code: "invalid_resource_policy", message: "every policy field is required"}))
		return
	}
	update := workergroup.ResourcePolicyUpdate{ExpectedRevision: *body.ExpectedRevision, ResourcePolicy: workergroup.ResourcePolicy{
		Optional: workergroup.OptionalResources{CPUMillis: *body.Optional.CPUMillis, MemoryBytes: *body.Optional.MemoryBytes, DiskBytes: *body.Optional.DiskBytes, VMSlots: *body.Optional.VMSlots, MaxTasks: *body.Optional.MaxTasks, IOBytesPerSecond: *body.Optional.IOBytesPerSecond}, LocalHeadroomSlots: *body.LocalHeadroomSlots}}
	policy, err := workergroup.UpdateResourcePolicy(r.Context(), s.tx, audit.CapacityAuthority(), group, pool, update)
	if err != nil {
		var input workergroup.InputError
		if errors.As(err, &input) {
			writeError(w, badRequest(codedError{code: "invalid_resource_policy", message: err.Error()}))
			return
		}
		s.writeWorkerGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}
