package computerhost

import (
	"context"
	"errors"
	"io"

	"net"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/httpclient"

	"github.com/helmrdotdev/helmr/internal/disk"
)

type computerSaveCapture interface {
	disk.CapturedVersion
	Adopt(context.Context, int) error
	Collect(context.Context, int) (int64, error)
}

func saveAdmissionRetryable(err error) bool {
	var response *httpclient.Error
	if errors.As(err, &response) {
		return response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
