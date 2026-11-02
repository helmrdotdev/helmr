package controlplane

import (
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func storageScope(in workerapi.StorageScope) (computer.StorageScope, error) {
	var scope computer.StorageScope
	if in.Computer != nil {
		identity, err := allocationIdentity(*in.Computer)
		if err != nil || identity.Kind != "computer" {
			return scope, computer.ErrInvalidInput
		}
		scope.Computer = &computer.LeaseIdentity{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch}
	}
	if in.Preparation != nil {
		ref, err := preparationExecutor(*in.Preparation)
		if err != nil {
			return scope, computer.ErrInvalidInput
		}
		scope.Preparation = &ref
	}
	if in.BootSeed != nil {
		ref, err := bootSeedExecutor(*in.BootSeed)
		if err != nil {
			return scope, computer.ErrInvalidInput
		}
		scope.BootSeed = &ref
	}
	if in.PreparationSource != nil {
		env, e1 := uuid.Parse(in.PreparationSource.EnvironmentID)
		build, e2 := uuid.Parse(in.PreparationSource.BuildID)
		if e1 != nil || e2 != nil {
			return scope, computer.ErrInvalidInput
		}
		scope.PreparationSource = &computer.PreparationKey{EnvironmentID: env, BuildID: build}
	}
	// The broker requires exactly one authority.
	return scope, nil
}

func storageDescriptor(in workerapi.CASObject) cas.Descriptor {
	return cas.Descriptor{Digest: in.Digest, SizeBytes: in.SizeBytes, MediaType: in.MediaType}
}

func (s *Server) workerStorageReads(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StorageReadRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	grant, err := s.storageGrants.Read(r.Context(), workerFromContext(r.Context()), scope, request.Store, request.Digest)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.StorageReadGrant{Digest: grant.Digest, Get: grant.Get, Head: grant.Head})
}

func (s *Server) workerStorageWrite(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StorageWriteRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	grant, err := s.storageGrants.Write(r.Context(), workerFromContext(r.Context()), scope, storageDescriptor(request.Object))
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.StorageWriteResponse{Exists: grant.Exists, Put: grant.Put, Upload: grant.Upload})
}

func (s *Server) workerStorageParts(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StoragePartsRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	parts, err := s.storageGrants.Parts(r.Context(), workerFromContext(r.Context()), scope, storageDescriptor(request.Object), request.UploadID, request.Parts)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.StoragePartsResponse{Parts: parts})
}

func (s *Server) workerStorageComplete(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StorageCompleteRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	if err == nil {
		err = s.storageGrants.Complete(r.Context(), workerFromContext(r.Context()), scope, storageDescriptor(request.Object), request.UploadID, request.Parts)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerStorageAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StorageAbortRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	if err == nil {
		err = s.storageGrants.Abort(r.Context(), workerFromContext(r.Context()), scope, storageDescriptor(request.Object), request.UploadID)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerCheckpointKey(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointKeyRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	identity, err := allocationIdentity(request.Identity)
	checkpoint, parseErr := uuid.Parse(request.CheckpointID)
	if err != nil || parseErr != nil || identity.Kind != "computer" {
		s.writeAllocationError(w, r, computer.ErrInvalidInput)
		return
	}
	key, err := s.allocationKeys.CheckpointKey(r.Context(), workerFromContext(r.Context()), computerAllocationIdentity(identity), checkpoint)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(key)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.ObjectKey{Key: key})
}

func (s *Server) workerBootSeedKey(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedKeyRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	scope, err := storageScope(request.Scope)
	build, parseErr := uuid.Parse(request.BuildID)
	if err != nil || parseErr != nil {
		s.writeAllocationError(w, r, computer.ErrInvalidInput)
		return
	}
	key, err := s.allocationKeys.BootSeedKey(r.Context(), workerFromContext(r.Context()), scope, build)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(key)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.ObjectKey{Key: key})
}
