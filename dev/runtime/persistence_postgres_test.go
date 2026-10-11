package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// These observations verify recorded ownership through the real psql syntax.
// They do not replace VM capture, source shutdown, or guest activation evidence.
func persistenceObservation(t *testing.T, f agenttest.Fixture, name string, session uuid.UUID) map[string]any {
	t.Helper()
	query, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "psql", f.Pool.Config().ConnString(), "-X", "-v", "ON_ERROR_STOP=1", "-v", "session_id="+session.String(), "-At")
	command.Env = append(os.Environ(), "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=5000")
	command.Stdin = strings.NewReader(string(query))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("observation %s: %v\n%s", name, err, output)
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return nil
	}
	var value map[string]any
	if err = json.Unmarshal(output, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPersistenceObservationQuery(t *testing.T) {
	f := agenttest.New(t)
	for _, name := range []string{"persistence.sql", "capture_abort.sql"} {
		if got := persistenceObservation(t, f, name, uuid.NewV7()); got != nil {
			t.Fatalf("absent Session produced evidence: %v", got)
		}
		if got := persistenceObservation(t, f, name, f.Session); got["checkpoint_id"] != nil || got["session_status"] != "open" {
			t.Fatalf("uncaptured Session: %v", got)
		}
	}
	save, cp := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest,manifest,vm_platform_id,ready_at) SELECT environment_id,$1,computer_id,epoch,1,$2,'ready','request',now()+interval '1 hour',decode(repeat('01',32),'hex'),'manifest',vm_platform_id,now() FROM computer_leases`, cp, save)
	// A Computer checkpoint is not evidence for an unrecorded member.
	if got := persistenceObservation(t, f, "persistence.sql", f.Session); got["checkpoint_id"] != nil {
		t.Fatalf("inferred membership: %v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_members(environment_id,checkpoint_id,session_id,process_epoch,computer_id) VALUES($1,$2,$3,1,$4)`, f.Environment, cp, f.Session, f.Computer)
	got := persistenceObservation(t, f, "persistence.sql", f.Session)
	if got["checkpoint_id"] != cp.String() || got["source_fenced"] != false || got["target_current"] != false {
		t.Fatalf("source still resident: %v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='released',fenced_at=now(),fence_evidence='source stopped'`)
	got = persistenceObservation(t, f, "persistence.sql", f.Session)
	if got["checkpoint_status"] != "ready" || got["source_fenced"] != true || got["target_runtime_id"] != nil {
		t.Fatalf("parked: %v", got)
	}
	instance := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,now()+interval '1 hour','active',$1,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,now(),now() FROM computer_leases WHERE epoch=1`, instance)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='consumed',target_lease_epoch=2,restore_control_version=2,controls_reconciled_at=now(),capture_request=NULL`)
	got = persistenceObservation(t, f, "persistence.sql", f.Session)
	if got["target_current"] != false {
		t.Fatalf("process not rebound: %v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET computer_lease_epoch=2`)
	got = persistenceObservation(t, f, "persistence.sql", f.Session)
	if got["target_current"] != true || got["target_runtime_id"] != instance.String() || got["controls_reconciled"] != true {
		t.Fatalf("restored: %v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=2`)
	if got = persistenceObservation(t, f, "persistence.sql", f.Session); got["target_current"] != false {
		t.Fatalf("stale host: %v", got)
	}
}

func TestSourceAbortObservationQuery(t *testing.T) {
	f := agenttest.New(t)
	save, cp := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_expires_at,capture_digest,abort_identity,controls_reconciled_at) VALUES($1,$2,$3,1,1,$4,'consumed',now()+interval '1 hour',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),now())`, f.Environment, cp, f.Computer, save)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_members(environment_id,checkpoint_id,session_id,process_epoch,computer_id) VALUES($1,$2,$3,1,$4)`, f.Environment, cp, f.Session, f.Computer)
	got := persistenceObservation(t, f, "capture_abort.sql", f.Session)
	if got["source_abort"] != true || got["acknowledged"] != true || got["source_fenced"] != false || got["lease_on_source"] != true || got["source_instance_id"] != f.Computer.String() {
		t.Fatalf("abort source: %v", got)
	}
	// A later capture must not be mistaken for the earlier acknowledged abort.
	newer := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest) VALUES($1,$2,$3,1,2,$4,'capturing','request',now()+interval '1 hour',decode(repeat('03',32),'hex'))`, f.Environment, newer, f.Computer, save)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_members(environment_id,checkpoint_id,session_id,process_epoch,computer_id) VALUES($1,$2,$3,1,$4)`, f.Environment, newer, f.Session, f.Computer)
	got = persistenceObservation(t, f, "capture_abort.sql", f.Session)
	if got["checkpoint_id"] != newer.String() || got["source_abort"] != false || got["acknowledged"] != false {
		t.Fatalf("old abort substituted: %v", got)
	}
}
