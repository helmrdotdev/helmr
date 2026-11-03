package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// workerRelease names the Control Plane's own release for its worker hosts.
func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if !release.ValidVersion(version.Version) {
		writeJSON(w, http.StatusOK, workerapi.ReleaseResponse{})
		return
	}
	published, err := workergroup.GetPublishedRelease(r.Context(), s.tx, version.Version)
	if errors.Is(err, workergroup.ErrReleaseNotPublished) {
		writeError(w, unavailable(codedError{code: "release_not_published", message: err.Error(), retryable: true}))
		return
	}
	if err != nil {
		s.writeWorkerHostError(w, "read worker release", err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ReleaseResponse{Release: &workerapi.Release{
		Version:   published.Version,
		Index:     workerapi.ReleaseObject(published.Index),
		Signature: workerapi.ReleaseObject(published.Signature),
	}})
}

// workerReleaseRead grants a read of one object of a published release.
func (s *Server) workerReleaseRead(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ReleaseReadRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid release read request JSON: %w", err))
		return
	}
	grant, err := s.storageGrants.ReleaseRead(r.Context(), request.Digest)
	if err != nil {
		s.writeAllocationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StorageReadGrant{Digest: grant.Digest, Get: grant.Get, Head: grant.Head})
}
