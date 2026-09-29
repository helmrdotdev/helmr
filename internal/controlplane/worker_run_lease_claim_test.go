package controlplane

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func discardTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWorkerRunLeaseClaimAuthorizesTransitionsAndReplays(t *testing.T) {
	server, f, work, worker, body, _ := newWorkerRunLeaseClaimHTTPFixture(t)
	handler := http.HandlerFunc(server.workerClaimRunLease)
	first := runWorkerLeaseClaimRequest(handler, worker, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first claim=%d %s", first.Code, first.Body)
	}
	var receipt string
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	replay := runWorkerLeaseClaimRequest(handler, worker, body)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body)
	}
	var before, after workerapi.RunLeaseClaimResponse
	if err := json.Unmarshal(first.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if before.Lease.ID != after.Lease.ID || before.Lease.LeaseSequence != after.Lease.LeaseSequence || before.Computer.WriteCapability != after.Computer.WriteCapability || !bytes.Equal(before.ProgramStart, after.ProgramStart) {
		t.Fatal("replay changed execution authority")
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='starting' AND to_jsonb(l)::text=$2 FROM run_leases l WHERE id=$1`, work.LeaseID, receipt).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("replay changed lease: %v %v", unchanged, err)
	}
}

func TestWorkerRunLeaseClaimRemainsReplayableAfterProjectionFailure(t *testing.T) {
	server, f, work, worker, body, store := newWorkerRunLeaseClaimHTTPFixture(t)
	handler := http.HandlerFunc(server.workerClaimRunLease)
	store.fail = true
	failed := runWorkerLeaseClaimRequest(handler, worker, body)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("projection failure=%d %s", failed.Code, failed.Body)
	}
	var claimed time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT claimed_at FROM run_leases WHERE id=$1 AND status='starting'`, work.LeaseID).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	store.fail = false
	replay := runWorkerLeaseClaimRequest(handler, worker, body)
	if replay.Code != http.StatusOK {
		t.Fatalf("retry=%d %s", replay.Code, replay.Body)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='starting' AND claimed_at=$2 FROM run_leases WHERE id=$1`, work.LeaseID, claimed).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("projection retry changed claim: %v %v", unchanged, err)
	}
}

func newWorkerRunLeaseClaimHTTPFixture(t *testing.T) (*Server, runtest.Fixture, runtest.RunLease, workerActor, []byte, *claimHTTPPlatformStore) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	raw := []byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := definition.CanonicalManifestAndDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, raw, digest[:])
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET payload=NULL WHERE id=$1`, work.RunID)
	key, err := computer.NewFencingKey(make([]byte, computer.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	var instanceID, computerID uuid.UUID
	var generation int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,computer_id,writer_generation FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID, &computerID, &generation); err != nil {
		t.Fatal(err)
	}
	capability, err := key.Derive(computer.FenceInput{InstanceID: instanceID, ComputerID: computerID, WriterGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(capability.Hash, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, instanceID, hash)
	store := &claimHTTPPlatformStore{checkCommit: func(ctx context.Context) error {
		var committed bool
		if err := f.Pool.QueryRow(ctx, `SELECT status='starting' AND claimed_at IS NOT NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&committed); err != nil {
			return err
		}
		if !committed {
			return errors.New("projection read uncommitted claim")
		}
		return nil
	}}
	server := &Server{tx: f.Pool, db: db.New(f.Pool), log: discardTestLogger(), platformStore: store, secretDelivery: claimHTTPSecrets{}, computerFencingKey: key}
	worker := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	body := []byte(`{"lease_id":"` + pgvalue.UUIDString(pgvalue.UUID(work.LeaseID)) + `","lease_sequence":1}`)
	return server, f, work, worker, body, store
}

type claimHTTPPlatformStore struct {
	fail        bool
	checkCommit func(context.Context) error
}

func (s *claimHTTPPlatformStore) Stat(ctx context.Context, digest string) (cas.Object, error) {
	if err := s.checkCommit(ctx); err != nil {
		return cas.Object{}, err
	}
	if s.fail {
		return cas.Object{}, errors.New("projection unavailable")
	}
	return cas.Object{Digest: digest, SizeBytes: 4096, MediaType: artifact.RuntimeArtifactMediaType}, nil
}
func (*claimHTTPPlatformStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected artifact read")
}

type claimHTTPSecrets struct{}

func (claimHTTPSecrets) OpenDeliveries(uuid.UUID, []secret.DeliveryEnvelope) ([]secret.DeliveryMaterial, error) {
	return nil, nil
}

func runWorkerLeaseClaimRequest(handler http.Handler, worker workerActor, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/leases/claim", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
