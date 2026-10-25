package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func bootSeedExecutor(in workerapi.BootSeedExecutor) (computer.BootSeedExecutor, error) {
	identity, err := allocationIdentity(in.Identity)
	if err != nil || identity.Kind != "seed" {
		return computer.BootSeedExecutor{}, computer.ErrInvalidInput
	}
	ref := computer.BootSeedExecutor{EnvironmentID: identity.EnvironmentID, BuildID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch, ChannelCredential: in.ChannelCredential}
	if !ref.Valid() {
		return computer.BootSeedExecutor{}, computer.ErrInvalidInput
	}
	return ref, nil
}

func (s *Server) workerDeliverBootSeed(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "seed")
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	result, err := s.allocator.DeliverBootSeed(r.Context(), workerFromContext(r.Context()), identity)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.BootSeedAllocationDelivery{Identity: allocationWireReceipt("seed", result.Allocation), Shape: allocationWireShape(result.Shape), ExpiresAt: *result.ExpiresAt, ChannelCredential: result.Executor.ChannelCredential})
}

func (s *Server) workerBootSeedStart(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := bootSeedExecutor(request)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	host := workerFromContext(r.Context())
	start, err := computer.ReadBootSeedStart(r.Context(), s.tx, host, ref)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	bundle, err := s.readExecutableBundle(r.Context(), start.BundleDigest)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	program, err := runtimeProgram(start.DeploymentID.String(), bundle)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	source, err := s.allocationKeys.BootSeedSource(r.Context(), host, ref)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer source.Clear()
	if err := computer.StartBootSeedBuild(r.Context(), s.tx, host, ref); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	response := workerapi.ComputerAllocationSource{BootProgram: &program, RootfsDigest: start.RootfsDigest, ImageConfig: start.ImageConfig,
		Disk: workerapi.ComputerSourceMaterial{VersionID: source.BaseVersion, Root: source.Root}}
	for _, key := range source.Keys {
		response.Disk.Keys = append(response.Disk.Keys, workerapi.ComputerKeyMaterial{Scope: source.Scope, ID: key.ID.String(), Key: key.Key})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerRenewBootSeed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := bootSeedExecutor(request)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	expires, err := computer.RenewBootSeedBuild(r.Context(), s.tx, workerFromContext(r.Context()), ref)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PreparationRenewal{ExpiresAt: expires})
}

func (s *Server) workerPublishBootSeed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedPublication
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := bootSeedExecutor(request.Executor)
	if err == nil {
		err = s.bootSeedPublisher.Publish(r.Context(), workerFromContext(r.Context()), ref, request.Manifest, request.Qualification)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerFailBootSeed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedFailure
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := bootSeedExecutor(request.Executor)
	if err == nil {
		err = computer.FailBootSeedBuild(r.Context(), s.tx, workerFromContext(r.Context()), ref, request.Code)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerReclaimBootSeed(w http.ResponseWriter, r *http.Request) {
	var request workerapi.BootSeedExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := bootSeedExecutor(request)
	if err == nil {
		err = computer.ReclaimBootSeedBuild(r.Context(), s.tx, workerFromContext(r.Context()), ref)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerBootSeedStopped(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "seed")
	if err == nil {
		err = dispatch.ObserveBootSeedStopped(r.Context(), s.tx, workerFromContext(r.Context()), identity)
	}
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
