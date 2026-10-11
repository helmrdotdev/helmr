package workergroup

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func retainedPreparation(t *testing.T, f agenttest.Fixture) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,status,deadline_at,error_code,executor_epoch,worker_host_id,worker_epoch,instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT $1,$2,$3,'failed-but-owned','failed',clock_timestamp()-interval '1 hour','fixture_failure',1,id,1,$4,decode(repeat('00',32),'hex'),1000,536870912,1073741824,vm_platform_id,1,cpu_environment_digest FROM worker_hosts WHERE id=$5`, f.Environment, id, f.Deployment, uuid.NewV7(), f.Worker)
	return id
}
func hostSecret(t *testing.T, f agenttest.Fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_host_secrets(id,worker_group_id,worker_host_id,key_prefix,secret_hash) VALUES($1,$2,$3,'fixture',decode(repeat('12',32),'hex'))`, uuid.NewV7(), f.Group, f.Worker)
}
func fenceFixtureComputer(t *testing.T, f agenttest.Fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1`, f.Environment)
}
func fenceFixturePreparation(t *testing.T, f agenttest.Fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1`, f.Environment)
}

func TestHostDrainWaitsForAllPhysicalCustodyAcrossEpochs(t *testing.T) {
	for _, kind := range []string{"computer", "preparation", "both"} {
		t.Run(kind, func(t *testing.T) {
			f := agenttest.New(t)
			hostSecret(t, f)
			if kind != "computer" {
				retainedPreparation(t, f)
			}
			if kind == "preparation" {
				fenceFixtureComputer(t, f)
			}
			// The current service must not hide a previous epoch's custody.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='draining',current_epoch=2,draining_at=clock_timestamp(),drain_reason='shutdown' WHERE id=$1`, f.Worker)
			principal := HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 2, HostClaimVersion: 1, GroupClaimVersion: 1}
			var conflict ConflictError
			if _, err := CompleteHostDrain(t.Context(), f.Pool, principal); !errors.As(err, &conflict) {
				t.Fatalf("drain discarded %s custody: %v", kind, err)
			}
			if kind == "both" {
				fenceFixtureComputer(t, f)
				if _, err := CompleteHostDrain(t.Context(), f.Pool, principal); !errors.As(err, &conflict) {
					t.Fatalf("drain discarded remaining preparation: %v", err)
				}
			}
			var active bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT revoked_at IS NULL FROM worker_host_secrets WHERE worker_host_id=$1`, f.Worker).Scan(&active); err != nil || !active {
				t.Fatalf("incomplete drain revoked cleanup credentials: %v %v", active, err)
			}
			fenceFixtureComputer(t, f)
			fenceFixturePreparation(t, f)
			completed, err := CompleteHostDrain(t.Context(), f.Pool, principal)
			if err != nil || completed.Status != "termination_ready" || completed.ClaimVersion != 2 {
				t.Fatalf("completed=%+v %v", completed, err)
			}
			replay, err := CompleteHostDrain(t.Context(), f.Pool, principal)
			if err != nil || replay.TerminationReadyAt != completed.TerminationReadyAt || replay.ClaimVersion != completed.ClaimVersion {
				t.Fatalf("replay=%+v %v", replay, err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT revoked_at IS NULL FROM worker_host_secrets WHERE worker_host_id=$1`, f.Worker).Scan(&active); err != nil || active {
				t.Fatalf("completed drain retained credentials: %v %v", active, err)
			}
		})
	}
}

func TestHostLossRevokesCredentialsWithoutReleasingPhysicalCustody(t *testing.T) {
	for _, path := range []string{"provider drift", "worker fence", "stale observation"} {
		t.Run(path, func(t *testing.T) {
			f := agenttest.New(t)
			hostSecret(t, f)
			retainedPreparation(t, f)
			q := db.New(f.Pool)
			var err error
			switch path {
			case "provider drift":
				_, err = MarkHostLost(t.Context(), f.Pool, f.Group, "host", 1)
			case "worker fence":
				err = FenceHost(t.Context(), q, HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1}, "provider_termination")
			case "stale observation":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.Worker)
				_, err = q.RecheckAndFenceStaleWorkerHost(t.Context(), db.RecheckAndFenceStaleWorkerHostParams{ID: pgvalue.UUID(f.Worker), WorkerGroupID: pgvalue.UUID(f.Group), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, RegistrationStaleBefore: pgvalue.Timestamptz(time.Now()), ObservationFreshnessSeconds: ObservationFreshnessSeconds})
			}
			if err != nil {
				t.Fatal(err)
			}
			var kept bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT h.status='lost' AND s.revoked_at IS NOT NULL
 AND EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$2 AND fenced_at IS NULL)
 AND EXISTS(SELECT 1 FROM computer_preparations WHERE environment_id=$2 AND fenced_at IS NULL)
 FROM worker_hosts h JOIN worker_host_secrets s ON s.worker_host_id=h.id WHERE h.id=$1`, f.Worker, f.Environment).Scan(&kept); err != nil || !kept {
				t.Fatalf("loss released physical custody or kept credentials: %v %v", kept, err)
			}
		})
	}
}

func TestGroupDisableWaitsForLostHostCustody(t *testing.T) {
	f := agenttest.New(t)
	retainedPreparation(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='draining' WHERE id=$1;
 UPDATE worker_pools SET status='disabled' WHERE worker_group_id=$1;
 UPDATE worker_hosts SET status='lost',current_epoch=2,lost_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.Group, f.Worker)
	var conflict ConflictError
	if _, err := DisableGroup(t.Context(), f.Pool, f.Group, 1); !errors.As(err, &conflict) {
		t.Fatalf("disabled with physical custody: %v", err)
	}
	fenceFixtureComputer(t, f)
	if _, err := DisableGroup(t.Context(), f.Pool, f.Group, 1); !errors.As(err, &conflict) {
		t.Fatalf("disabled with preparation custody: %v", err)
	}
	fenceFixturePreparation(t, f)
	result, err := DisableGroup(t.Context(), f.Pool, f.Group, 1)
	if err != nil || result.Status != "disabled" {
		t.Fatalf("group disable=%+v %v", result, err)
	}
}
