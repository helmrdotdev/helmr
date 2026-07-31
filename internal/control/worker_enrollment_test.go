package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestWorkerEnrollmentValidatesNonceBeforeExactProofVerification(t *testing.T) {
	rawRequest := json.RawMessage(" \n{\"worker_group_id\":\"run-workers\",\"nonce\":\"fresh-nonce\"}\n")

	t.Run("invalid nonce does not verify", func(t *testing.T) {
		verifier := &recordingEnrollmentVerifier{}
		server := testEnrollmentServer(t, enrollmentNonceStore{active: false}, verifier, io.Discard)
		response := serveWorkerEnrollment(server, rawRequest)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		if verifier.verifyCalls != 0 {
			t.Fatalf("verify calls = %d", verifier.verifyCalls)
		}
		if !bytes.Equal(verifier.parsed, rawRequest) {
			t.Fatalf("parse bytes = %q, want %q", verifier.parsed, rawRequest)
		}
	})

	t.Run("valid nonce verifies the same bytes without logging proof error", func(t *testing.T) {
		verifier := &recordingEnrollmentVerifier{}
		var logs bytes.Buffer
		server := testEnrollmentServer(t, enrollmentNonceStore{active: true}, verifier, &logs)
		response := serveWorkerEnrollment(server, rawRequest)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		if verifier.verifyCalls != 1 {
			t.Fatalf("verify calls = %d", verifier.verifyCalls)
		}
		if !bytes.Equal(verifier.parsed, rawRequest) || !bytes.Equal(verifier.verified, rawRequest) {
			t.Fatalf("parse bytes = %q verify bytes = %q, want %q", verifier.parsed, verifier.verified, rawRequest)
		}
		if bytes.Contains(logs.Bytes(), []byte("sensitive-proof-value")) {
			t.Fatalf("proof-derived verifier error was logged: %s", logs.String())
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

type recordingEnrollmentVerifier struct {
	parsed      json.RawMessage
	verified    json.RawMessage
	verifyCalls int
}

func (v *recordingEnrollmentVerifier) ParseWorkerEnrollment(raw json.RawMessage) (api.WorkerEnrollmentIntent, error) {
	v.parsed = bytes.Clone(raw)
	return api.WorkerEnrollmentIntent{
		WorkerGroupID: "run-workers", Nonce: "fresh-nonce", SupportsRun: true,
		ProtocolVersion: auth.WorkerProtocolVersion,
	}, nil
}

func (v *recordingEnrollmentVerifier) VerifyWorkerEnrollment(_ context.Context, raw json.RawMessage) (VerifiedWorkerEnrollment, error) {
	v.verifyCalls++
	v.verified = bytes.Clone(raw)
	return VerifiedWorkerEnrollment{}, errors.New("sensitive-proof-value")
}

func testEnrollmentServer(t *testing.T, store enrollmentNonceStore, verifier *recordingEnrollmentVerifier, logOutput io.Writer) *Server {
	t.Helper()
	keys, err := auth.NewKeys(make([]byte, auth.RootKeySize))
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

func serveWorkerEnrollment(server *Server, raw json.RawMessage) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/worker/enrollment", bytes.NewReader(raw))
	response := httptest.NewRecorder()
	server.workerEnroll(response, request)
	return response
}
