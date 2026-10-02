package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestWorkerObservationRecoversAfterHungRequest(t *testing.T) {
	for _, draining := range []bool{false, true} {
		name := "periodic"
		if draining {
			name = "drain completion"
		}
		t.Run(name, func(t *testing.T) {
			var observations atomic.Int32
			canceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch r.URL.Path {
				case "/worker/v1/instance/credential":
					_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "credential", ExpiresInSeconds: 3600})
				case "/worker/v1/instance/observations":
					if observations.Add(1) == 1 {
						<-r.Context().Done()
						close(canceled)
						return
					}
					_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusDraining})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := workerclient.New(server.URL, workerclient.WithAuth("host", "secret"), workerclient.WithService("service"))
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(Config{Recover: emptyPhysicalRecovery, ControlPlane: client, ObservationEvery: 100 * time.Millisecond, PollEvery: 10 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			statuses := make(chan workerapi.StatusResponse, 1)
			fatalWork := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if draining {
					if err := s.waitForDrainReady(ctx, RecoveryEvidence{}, fatalWork); err != nil {
						fatalWork <- err
					} else {
						statuses <- workerapi.StatusResponse{Status: workerapi.StatusDraining}
					}
					cancel()
				} else {
					s.observe(ctx, RecoveryEvidence{}, func(status workerapi.StatusResponse) { statuses <- status; cancel() }, fatalWork)
				}
			}()
			defer func() {
				cancel()
				if draining {
					select {
					case fatalWork <- errors.New("test cleanup"):
					default:
					}
				}
				<-done
			}()
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("hung observation never canceled")
			}
			select {
			case status := <-statuses:
				if status.Status != workerapi.StatusDraining {
					t.Fatalf("status = %+v", status)
				}
			case err := <-fatalWork:
				t.Fatalf("observation timeout treated as authority rejection: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("observations did not resume after hung request")
			}

		})
	}
}
