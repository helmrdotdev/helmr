package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestWorkerDiscoverRunLeasesReturnsExactTuples(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	store := &runLeaseDiscoveryStore{
		rows: []db.DiscoverWorkerRunLeaseWorkRow{{
			ID:            pgvalue.UUID(leaseID),
			LeaseSequence: 4,
		}},
	}
	server := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  store,
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/leases/discover", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerInstanceID: workerID,
		WorkerGroupID:    "run-workers",
		WorkerEpoch:      9,
	}))
	response := httptest.NewRecorder()

	server.workerDiscoverRunLeases(response, request)

	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), leaseID.String()) ||
		!strings.Contains(response.Body.String(), `"lease_sequence":4`) {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func TestWorkerDiscoverRunLeasesRejectsAuthorityFields(t *testing.T) {
	server := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  &runLeaseDiscoveryStore{},
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/worker/v1/run/leases/discover",
		strings.NewReader(`{"lease_id":"not-authority"}`),
	)
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{}))
	response := httptest.NewRecorder()

	server.workerDiscoverRunLeases(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}
