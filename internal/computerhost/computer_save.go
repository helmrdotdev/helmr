package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io"
	"os"

	"net"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/httpclient"

	"github.com/helmrdotdev/helmr/internal/disk"
)

type computerSaveCapture = disk.RetainedVersion

func saveAdmissionRetryable(err error) bool {
	if response, ok := errors.AsType[*httpclient.Error](err); ok {
		return response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Save retries preserve local custody even when a provider refuses storage (for
// example S3 AccessDenied). Only positive authority loss, corruption or a broken
// local protocol ends this owner; HTTP status alone does not establish fencing.
func saveOperationRetryable(err error) bool {
	var rejected interface{ WorkerAuthorityRejected() bool }
	if errors.As(err, &rejected) && rejected.WorkerAuthorityRejected() {
		return false
	}
	var response *httpclient.Error
	if errors.As(err, &response) && (response.Code == workerapi.SaveAuthorityLost || response.Code == workerapi.AllocationClosed || response.StatusCode == http.StatusBadRequest) {
		return false
	}
	if errors.Is(err, cas.ErrRemoteIntegrity) {
		return true
	}
	var fatal interface{ FatalDeviceError() bool }
	var source *disk.SourceFailure
	var protocol *saveProtocolError
	return !errors.As(err, &protocol) && !errors.As(err, &source) && !(errors.As(err, &fatal) && fatal.FatalDeviceError()) && !errors.Is(err, blockformat.ErrIntegrity) && !errors.Is(err, blockformat.ErrAuthentication) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, cas.ErrDigestMismatch)
}

type saveProtocolError struct{ cause error }

func (e *saveProtocolError) Error() string { return e.cause.Error() }
func (e *saveProtocolError) Unwrap() error { return e.cause }
