package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/region"
)

// regionError maps errors of the region owner to HTTP errors. Errors it does
// not describe are returned unchanged for the caller to log and report as
// internal.
func regionError(err error) error {
	var input region.InputError
	switch {
	case errors.As(err, &input):
		return badRequest(err)
	case errors.Is(err, region.ErrNotFound):
		return notFound(err)
	case errors.Is(err, region.ErrExists):
		return conflict(err)
	default:
		return err
	}
}

// writeRegionError writes a region owner error, logging the failures it does
// not describe to the client.
func (s *Server) writeRegionError(w http.ResponseWriter, err error) {
	mapped := regionError(err)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("region request failed", "error", err)
	writeError(w, errors.New("region request"))
}
