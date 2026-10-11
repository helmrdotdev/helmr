package workerclient

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestDiagnosticLogClientsPreserveTruncatedRejection(t *testing.T) {
	for _, name := range []string{"session", "preparation"} {
		t.Run(name, func(t *testing.T) {
			for _, status := range []int{409, 410, 503} {
				calls := 0
				client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(preparationBrokenBody{}), Header: make(http.Header)}, nil
				})})
				var err error
				if name == "session" {
					_, err = client.AppendSessionLog(context.Background(), workerapi.SessionLogRequest{})
				} else {
					_, err = client.AppendPreparationLog(context.Background(), workerapi.PreparationLogRequest{})
				}
				if calls != 1 || !httpclient.IsStatus(err, status) {
					t.Fatalf("status=%d calls=%d error=%v", status, calls, err)
				}
			}
		})
	}
}
