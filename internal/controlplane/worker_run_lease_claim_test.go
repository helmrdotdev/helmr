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
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func discardTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWorkerRunLeaseClaimAuthorizesTransitionsAndReplays(t *testing.T) {
	f, work, worker, body, _ := newWorkerRunLeaseClaimHTTPFixture(t)
	first := worker.send(t, workerRunLeaseClaimPath, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first claim=%d %s", first.Code, first.Body)
	}
	var receipt string
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	replay := worker.send(t, workerRunLeaseClaimPath, body)
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
	f, work, worker, body, store := newWorkerRunLeaseClaimHTTPFixture(t)
	store.fail = true
	failed := worker.send(t, workerRunLeaseClaimPath, body)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("projection failure=%d %s", failed.Code, failed.Body)
	}
	var claimed time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT claimed_at FROM run_leases WHERE id=$1 AND status='starting'`, work.LeaseID).Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	store.fail = false
	replay := worker.send(t, workerRunLeaseClaimPath, body)
	if replay.Code != http.StatusOK {
		t.Fatalf("retry=%d %s", replay.Code, replay.Body)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='starting' AND claimed_at=$2 FROM run_leases WHERE id=$1`, work.LeaseID, claimed).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("projection retry changed claim: %v %v", unchanged, err)
	}
}

const workerRunLeaseClaimPath = "/worker/v1/run/leases/claim"

// newWorkerRunLeaseClaimHTTPFixture serves NewServer, reading platform
// artifacts from the returned store, to the worker host of an assigned lease
// whose Computer Instance holds the configured fencing key's writer token.
func newWorkerRunLeaseClaimHTTPFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerHTTPClient, json.RawMessage, *claimHTTPPlatformStore) {
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
	key, err := disk.NewFencingKey(make([]byte, disk.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	var instanceID, computerID uuid.UUID
	var generation int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,computer_id,writer_generation FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID, &computerID, &generation); err != nil {
		t.Fatal(err)
	}
	capability, err := key.Derive(disk.FenceInput{InstanceID: instanceID, ComputerID: computerID, WriterGeneration: generation})
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
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.PlatformStore = store
		cfg.ComputerFencingKey = key
	})
	body := json.RawMessage(`{"lease_id":"` + pgvalue.UUIDString(pgvalue.UUID(work.LeaseID)) + `","lease_sequence":1}`)
	return f, work, newWorkerHTTPClient(t, handler, f.Pool, f.WorkerID), body, store
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
