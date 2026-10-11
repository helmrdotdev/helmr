package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeAttachmentsAreMonotonicAcrossConcurrentRequests(t *testing.T) {
	f := newFixture(t)
	const count = 16
	type result struct {
		attachment RuntimeAttachment
		err        error
	}
	results := make(chan result, count)
	var jobs sync.WaitGroup
	for range count {
		jobs.Go(func() {
			attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			results <- result{attachment, err}
		})
	}
	jobs.Wait()
	seen := make(map[int64]bool)
	for range count {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		attachment := result.attachment
		if attachment.Sequence < 1 || attachment.Sequence > count || seen[attachment.Sequence] || attachment.Authority.Generation != 1 || !attachment.Authority.ExpiresAt.After(time.Now()) {
			t.Fatalf("invalid or reused attachment: %+v", attachment)
		}
		seen[attachment.Sequence] = true
	}
	if _, err := RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), f.execution()); err != nil {
		t.Fatal(err)
	}
	// Even if a prior acquisition's response was lost, the next connection
	// advances. Ordinary renewal does not consume a new attachment sequence.
	next, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil || next.Sequence != count+1 {
		t.Fatalf("next attachment: %+v %v", next, err)
	}
}

func TestRuntimeAttachmentPreservesHeldClosingSession(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, "UPDATE sessions SET status='closing',authority_generation=2")
	dbtest.MustExec(t, t.Context(), f.pool, "INSERT INTO session_holds(environment_id,id,session_id,scope,reason) SELECT environment_id,gen_random_uuid(),id,'local','test' FROM sessions")
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil || attachment.Sequence != 1 || attachment.Authority.Generation != 2 {
		t.Fatalf("control attachment: %+v %v", attachment, err)
	}
	var unchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='closing' AND authority_generation=2 AND EXISTS(
SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id AND h.released_at IS NULL)
FROM sessions s WHERE environment_id=$1 AND id=$2`, f.env, f.session).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("attachment changed lifecycle or hold: %v", err)
	}
}

func TestRuntimeAttachmentRejectsUnavailableExecution(t *testing.T) {
	for _, failure := range []string{"process", "lease", "host_epoch", "expired", "fenced", "claims", "exhausted"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			execution := f.execution()
			var expectedSequence int64
			switch failure {
			case "process":
				execution.ProcessEpoch++
			case "lease":
				execution.LeaseEpoch++
			case "host_epoch":
				execution.WorkerEpoch++
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'")
			case "fenced":
				dbtest.MustExec(t, t.Context(), f.pool, "UPDATE session_processes SET status='lost',fenced_at=clock_timestamp()")
			case "claims":
				dbtest.MustExec(t, t.Context(), f.pool, "UPDATE worker_hosts SET claim_version=2")
			case "exhausted":
				expectedSequence = 9223372036854775807
				dbtest.MustExec(t, t.Context(), f.pool, "UPDATE session_processes SET attachment_sequence=$1", expectedSequence)
			}
			attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), execution)
			if err == nil || attachment != (RuntimeAttachment{}) {
				t.Fatalf("unavailable execution acquired attachment: %+v %v", attachment, err)
			}
			if failure != "claims" && !errors.Is(err, ErrDenied) && !errors.Is(err, ErrNotReady) {
				t.Fatalf("untyped rejection: %v", err)
			}
			var sequence int64
			if err := f.pool.QueryRow(t.Context(), "SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=1", f.env, f.session).Scan(&sequence); err != nil || sequence != expectedSequence {
				t.Fatalf("rejection changed sequence to %d: %v", sequence, err)
			}
		})
	}
}

func TestRuntimeAttachmentRechecksLeaseAfterProcessLock(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, "UPDATE computer_leases SET expires_at=clock_timestamp()+interval '1 second'")
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), "SELECT epoch FROM session_processes FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%attachment_sequence%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attachment did not wait for process lock")
		}
		time.Sleep(time.Millisecond)
	}
	// Wait for the actual database deadline while the process row remains locked.
	for {
		var expired bool
		if err = f.pool.QueryRow(t.Context(), "SELECT expires_at<=clock_timestamp() FROM computer_leases").Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrNotReady) && !errors.Is(err, ErrDenied) {
		t.Fatalf("expired lease accepted: %v", err)
	}
	var sequence int64
	if err = f.pool.QueryRow(t.Context(), "SELECT attachment_sequence FROM session_processes").Scan(&sequence); err != nil || sequence != 0 {
		t.Fatalf("rejected attachment advanced sequence: %d %v", sequence, err)
	}
}

func TestRuntimeAttachmentRejectsOtherAndSupersededOwners(t *testing.T) {
	f := newFixture(t)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest)
 SELECT $1,'other-host',worker_group_id,worker_pool_id,status,current_epoch,$1,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest FROM worker_hosts WHERE id=$2`, other, f.worker)
	otherHost := *f.host()
	otherHost.HostID = other
	otherExecution := f.execution()
	otherExecution.WorkerHostID = other
	// All principal claims are current and agree with execution. Only the
	// actual Computer ownership differs.
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, otherHost, otherExecution)
	if !errors.Is(err, ErrNotReady) || attachment != (RuntimeAttachment{}) {
		t.Fatalf("other host acquired: %+v %v", attachment, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='source closed' WHERE epoch=1`)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT $1,$2,2,$3,1,clock_timestamp()+interval '1 hour','active',$4,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer, other, uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET computer_lease_epoch=2`)
	// Original host authentication remains valid after physical takeover.
	attachment, err = AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if !errors.Is(err, ErrNotReady) || attachment != (RuntimeAttachment{}) {
		t.Fatalf("superseded owner acquired: %+v %v", attachment, err)
	}
	var sequence int64
	if err = f.pool.QueryRow(t.Context(), "SELECT attachment_sequence FROM session_processes").Scan(&sequence); err != nil || sequence != 0 {
		t.Fatalf("denied owner advanced sequence: %d %v", sequence, err)
	}
	otherExecution.LeaseEpoch = 2
	attachment, err = AcquireRuntimeAttachment(t.Context(), f.pool, otherHost, otherExecution)
	if err != nil || attachment.Sequence != 1 {
		t.Fatalf("current owner rejected: %+v %v", attachment, err)
	}
}

func TestRuntimeAttachmentReportsStoppedWithoutGrant(t *testing.T) {
	for _, state := range []string{"owned", "expired", "wrong-lease", "wrong-host", "fenced-lease"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			e := f.execution()
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp(),attachment_sequence=7 WHERE session_id=$1`, f.session)
			switch state {
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
			case "wrong-lease":
				e.LeaseEpoch++
			case "wrong-host":
				e.WorkerHostID = uuid.NewV7()
			case "fenced-lease":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET fenced_at=clock_timestamp(),status='released',fence_evidence='physical closure'`)
			}
			for range 2 {
				result, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
				if state == "owned" {
					if err != nil || result != (RuntimeAttachment{Stopped: true}) {
						t.Fatalf("stopped observation: %+v %v", result, err)
					}
				} else if err == nil || result != (RuntimeAttachment{}) {
					t.Fatalf("unowned stopped observation: %+v %v", result, err)
				}
			}
			var sequence int64
			if err := f.pool.QueryRow(t.Context(), `SELECT attachment_sequence FROM session_processes WHERE session_id=$1`, f.session).Scan(&sequence); err != nil || sequence != 7 {
				t.Fatalf("stopped observation issued attachment: %d %v", sequence, err)
			}
			if _, err := RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), e); err == nil {
				t.Fatal("stopped observation became execution authority")
			}
		})
	}
}
