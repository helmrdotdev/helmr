package controlplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

const enrollmentTestSecret = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestWorkerEnrollmentChecksNonceBeforeProofVerification(t *testing.T) {
	request, err := enrollment.BuildRequest("run-workers", "fresh-nonce", true, false, "host-1", enrollmentTestSecret)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("inactive nonce rejects an otherwise valid proof", func(t *testing.T) {
		server := testEnrollmentServer(t, enrollmentNonceStore{active: false}, io.Discard)
		response := serveWorkerEnrollment(server, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("active nonce rejects invalid proof without logging it", func(t *testing.T) {
		var logs bytes.Buffer
		server := testEnrollmentServer(t, enrollmentNonceStore{active: true}, &logs)
		invalid := request
		invalid.Proof = "sensitive-proof-value"
		response := serveWorkerEnrollment(server, invalid)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		if bytes.Contains(logs.Bytes(), []byte(invalid.Proof)) {
			t.Fatalf("enrollment proof was logged: %s", logs.String())
		}
	})
}

type enrollmentNonceStore struct {
	db.Querier
	active bool
}

func (s enrollmentNonceStore) GetActiveWorkerEnrollmentNonce(context.Context, db.GetActiveWorkerEnrollmentNonceParams) (db.WorkerEnrollmentNonce, error) {
	if !s.active {
		return db.WorkerEnrollmentNonce{}, pgx.ErrNoRows
	}
	return db.WorkerEnrollmentNonce{WorkerGroupID: "run-workers"}, nil
}

func testEnrollmentServer(t *testing.T, store enrollmentNonceStore, logOutput io.Writer) *Server {
	t.Helper()
	keys, err := auth.NewKeys(make([]byte, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := enrollment.NewVerifier([]enrollment.GroupSecret{{GroupID: "run-workers", Secret: enrollmentTestSecret}})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		log:                   slog.New(slog.NewTextHandler(logOutput, nil)),
		db:                    store,
		authKeys:              keys,
		workerEnrollment:      verifier,
		workerEnrollmentGuard: newWorkerEnrollmentGuard(),
	}
}

func serveWorkerEnrollment(server *Server, request workerapi.EnrollmentRequest) *httptest.ResponseRecorder {
	body := bytes.NewBufferString(`{"worker_group_id":"` + request.WorkerGroupID + `","nonce":"` + request.Nonce + `","supports_run":true,"supports_build":false,"resource_id":"` + request.ResourceID + `","proof":"` + request.Proof + `"}`)
	httpRequest := httptest.NewRequest(http.MethodPost, "/api/worker/v0/enrollment", body)
	response := httptest.NewRecorder()
	server.workerEnroll(response, httpRequest)
	return response
}
