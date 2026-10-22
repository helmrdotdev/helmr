package controlplane

import (
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerPreparationSource(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationSourceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeAllocationError(w, err)
		return
	}
	env, err := uuid.Parse(request.EnvironmentID)
	if err != nil {
		s.writeAllocationError(w, computer.ErrInvalidInput)
		return
	}
	build, err := uuid.Parse(request.BuildID)
	if err != nil {
		s.writeAllocationError(w, computer.ErrInvalidInput)
		return
	}
	key := computer.PreparationKey{EnvironmentID: env, BuildID: build}
	host := workerFromContext(r.Context())
	metadata, err := s.allocationKeys.PreparationStart(r.Context(), host, key)
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	bundle, err := s.readExecutableBundle(r.Context(), metadata.BundleDigest)
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	program, err := runtimeProgram(metadata.DeploymentID.String(), bundle)
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	source, err := s.allocationKeys.PreparationSource(r.Context(), host, key)
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	defer source.Disk.Clear()
	content, err := source.Manifest.Content()
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	if content.Program == nil || program.Runtime != workerapi.CASObject(content.Program.Runtime) || program.Artifact != workerapi.CASObject(content.Program.Artifact) {
		s.writeAllocationError(w, computer.ErrConflict)
		return
	}
	response := workerapi.PreparationSource{WorkerHostID: source.HostID.String(), WorkerEpoch: source.HostEpoch,
		EnvironmentID: env.String(), BuildID: build.String(), PolicyRevision: source.PolicyRevision, ExpiresAt: source.ExpiresAt,
		Manifest: source.Manifest, Shape: allocationWireShape(source.Shape),
		Source: workerapi.ComputerAllocationSource{BootProgram: &program, RootfsDigest: source.RootfsDigest, ImageConfig: source.ImageConfig,
			Disk: workerapi.ComputerSourceMaterial{VersionID: source.Disk.BaseVersion, Root: source.Disk.Root}}}
	for _, key := range source.Disk.Keys {
		response.Source.Disk.Keys = append(response.Source.Disk.Keys, workerapi.ComputerKeyMaterial{Scope: source.Disk.Scope, ID: key.ID.String(), Key: key.Key})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}
