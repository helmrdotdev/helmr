package runtime

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computerdbtest"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func observe(t *testing.T, pool *pgxpool.Pool, name string, inputs map[string]any) any {
	t.Helper()
	encoded, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "python3", "observe.py", name, string(encoded))
	sql, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render observation: %v\n%s", err, sql)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	results, err := conn.Conn().PgConn().Exec(t.Context(), string(sql)).ReadAll()
	if err != nil {
		t.Fatalf("observation %s: %v", name, err)
	}
	var value any
	rows := 0
	for _, result := range results {
		for _, row := range result.Rows {
			if len(row) != 1 {
				t.Fatalf("observation emitted non-JSON shape: %v", row)
			}
			if err := json.Unmarshal(row[0], &value); err != nil {
				t.Fatal(err)
			}
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("observation emitted %d rows", rows)
	}
	return value
}

func TestVerificationObservations(t *testing.T) {
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, lease.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	placements := observe(t, f.Pool, "run-placements", map[string]any{"run_ids": []string{lease.RunID.String()}}).([]any)
	if len(placements) != 1 || placements[0].(map[string]any)["worker_host_id"] != f.WorkerID.String() {
		t.Fatalf("placements=%v", placements)
	}
	workers := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{f.WorkerID.String()}}).([]any)
	if len(workers) != 1 || workers[0].(map[string]any)["active_leases"] != float64(1) || workers[0].(map[string]any)["unreclaimed_instances"] != float64(1) {
		t.Fatalf("workers=%v", workers)
	}
	run := observe(t, f.Pool, "run-state", map[string]any{"run_id": lease.RunID.String()}).(map[string]any)
	if run["active_leases"] != float64(1) || run["unreclaimed_instances"] != float64(1) {
		t.Fatalf("run=%v", run)
	}
	computer := observe(t, f.Pool, "computer-state", map[string]any{"computer_id": computerID.String()}).(map[string]any)
	if computer["latest_run_id"] != lease.RunID.String() {
		t.Fatalf("computer=%v", computer)
	}
	path := observe(t, f.Pool, "run-path", map[string]any{"run_id": lease.RunID.String()}).(map[string]any)
	if len(path["leases"].([]any)) != 1 {
		t.Fatalf("path=%v", path)
	}
	if observe(t, f.Pool, "clock", map[string]any{}).(map[string]any)["epoch"].(float64) <= 0 {
		t.Fatal("missing clock")
	}
	var project, environment string
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.slug,e.slug FROM projects p JOIN environments e ON e.project_id=p.id WHERE e.id=$1`, f.EnvironmentID).Scan(&project, &environment); err != nil {
		t.Fatal(err)
	}
	deployments := observe(t, f.Pool, "deployment-state", map[string]any{"project": project, "environment": environment}).([]any)
	if len(deployments) != 1 {
		t.Fatalf("deployments=%v", deployments)
	}
	keys := observe(t, f.Pool, "api-key-scope", map[string]any{"key_prefix": "absent", "project": project, "environment": environment}).([]any)
	if len(keys) != 0 {
		t.Fatalf("keys=%v", keys)
	}
	// A stale epoch cannot supply current placement evidence.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, f.WorkerID)
	if got := observe(t, f.Pool, "run-placements", map[string]any{"run_ids": []string{lease.RunID.String()}}).([]any); len(got) != 0 {
		t.Fatalf("stale placements=%v", got)
	}
}

func TestVerificationObservationCurrentHost(t *testing.T) {
	f := runtest.New(t)
	stale := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,lost_at)
 SELECT $2,resource_id,worker_group_id,worker_pool_id,'lost',now() FROM worker_hosts WHERE id=$1`, f.WorkerID, stale)
	got := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{f.WorkerID.String()}}).([]any)
	if len(got) != 1 || got[0].(map[string]any)["id"] != f.WorkerID.String() {
		t.Fatalf("current host=%v", got)
	}
}

func TestVerificationObservationRejectsQueryInputs(t *testing.T) {
	for _, args := range [][]string{{"../persistence", `{}`}, {"run-state", `{"run_id":"x';DELETE FROM runs;--"}`}, {"clock", `{"sql":"DELETE FROM runs"}`}, {"run-placements", `{"run_ids":[]}`}, {"worker-state", `{"resource_ids":["host\nnewline"]}`}} {
		out, err := exec.CommandContext(t.Context(), "python3", append([]string{"observe.py"}, args...)...).CombinedOutput()
		if err == nil || strings.Contains(string(out), "BEGIN READ ONLY;") {
			t.Fatalf("invalid inputs rendered SQL: %q %s", args, out)
		}
	}
}

func TestVerificationObservationCheckpointLineage(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	_, newLease := seedObservationRestore(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET created_at=ready_at-interval '3 seconds' WHERE id=(SELECT source_checkpoint_id FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1))`, newLease)
	path := observe(t, f.Pool, "run-path", map[string]any{"run_id": work.RunID.String()}).(map[string]any)
	leases := path["leases"].([]any)
	if len(leases) != 2 {
		t.Fatalf("leases=%v", leases)
	}
	restored := leases[1].(map[string]any)
	if restored["id"] != newLease.String() || restored["inferred_path"] != "checkpoint_resume" || restored["has_checkpoint_resume"] != true {
		t.Fatalf("restored=%v", restored)
	}
	checkpoints := path["checkpoints"].([]any)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints=%v", checkpoints)
	}
	checkpoint := checkpoints[0].(map[string]any)
	if checkpoint["status"] != "ready" || checkpoint["id"] != restored["source_checkpoint_id"] || len(checkpoint["artifacts"].([]any)) != 4 {
		t.Fatalf("checkpoint=%v", checkpoint)
	}
	created, err := time.Parse(time.RFC3339Nano, checkpoint["created_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := time.Parse(time.RFC3339Nano, checkpoint["ready_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if ready.Sub(created) != 3*time.Second {
		t.Fatalf("checkpoint duration=%s", ready.Sub(created))
	}
	total := float64(0)
	for _, value := range checkpoint["artifacts"].([]any) {
		total += value.(map[string]any)["size_bytes"].(float64)
	}
	if total != 4 {
		t.Fatalf("checkpoint object bytes=%v", total)
	}
	// Checkpoint association alone cannot classify another Run as resumed.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET suspension_status='hot',prior_run_lease_id=NULL,suspend_checkpoint_id=NULL WHERE run_id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_checkpoint_runs WHERE run_id=$1`, work.RunID)
	unproven := observe(t, f.Pool, "run-path", map[string]any{"run_id": work.RunID.String()}).(map[string]any)["leases"].([]any)[1].(map[string]any)
	if unproven["has_checkpoint_resume"] != false {
		t.Fatalf("unproven=%v", unproven)
	}
}

func seedObservationRestore(t *testing.T, f runtest.Fixture, work runtest.RunLease) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	checkpoint, private, instance, newLease, waitID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	var computerID, sourceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id,computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &sourceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoints(id,computer_id,environment_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,base_computer_disk_version_id)
 SELECT $2,i.computer_id,i.environment_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,i.program_deployment_id,c.head_disk_version_id
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, sourceID, checkpoint)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,base_computer_disk_version_id,$3,4096,'private',source_computer_instance_id,writer_generation FROM computer_checkpoints WHERE id=$1`, checkpoint, private, dbtest.Digest("restored-private"))
	computerdbtest.InsertComputerVersion(t, ctx, tx, f.EnvironmentID, computerID, private)
	artifacts := computerdbtest.InsertCheckpointArtifacts(t, ctx, tx, work.RunID, "restore-timer")
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,
 ready_request_fingerprint=$3,manifest='{"version":1}',vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpoint, private, dbtest.Digest("restore-ready"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_instances SET desired_state='closed',desired_version=2,admission_state='closed',observed_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{}',terminal_reason_code='checkpointed' WHERE id=$1`, sourceID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='checkpointed',checkpointed_at=now(),terminal_at=now(),terminal_reason_code='checkpointed',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,
 vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,
 computer_id,program_deployment_id,preparation_expires_at,desired_reason,observed_state,ready_at,observed_version,observed_desired_version,
 writer_generation,writer_token_hash,writer_expires_at,admission_state,mount_state,mounted_at,source_checkpoint_id,source_disk_version_id)
 SELECT $2,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,worker_epoch,
 vm_platform_id,computer_spec_id,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,
 computer_id,program_deployment_id,now()+interval '5 minutes','restore','ready',now(),1,1,
 3,decode(repeat('03',32),'hex'),now()+interval '10 minutes','restoring','mounted',now(),$3,$4 FROM computer_instances WHERE id=$1`, sourceID, instance, checkpoint, private)
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET resume_computer_instance_id=$2,resume_committed_at=now() WHERE id=$1`, checkpoint, instance)
	dbtest.MustExec(t, ctx, tx, `UPDATE computers SET writer_generation=3 WHERE id=$1`, computerID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,
 computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
 status,start_deadline_at,claimed_at,started_at,expires_at)
 SELECT $2,org_id,project_id,environment_id,run_id,computer_id,region_id,2,attempt_number,worker_group_id,worker_host_id,worker_epoch,
 $3,3,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,
 'running',now()+interval '5 minutes',now(),now(),now()+interval '10 minutes' FROM run_leases WHERE id=$1`, work.LeaseID, newLease, instance)
	dbtest.MustExec(t, ctx, tx, `UPDATE runs SET status='waiting',current_run_lease_id=$2,active_started_at=now() WHERE id=$1`, work.RunID, newLease)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id,prior_run_lease_id,suspend_checkpoint_id,suspension_status)
 SELECT $2,environment_id,id,computer_id,'timer',now()-interval '1 second',revision,1,$3,$4,$5,'resuming' FROM runs WHERE id=$1`, work.RunID, waitID, newLease, work.LeaseID, checkpoint)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_computer_instance_id,writer_generation)
 SELECT $2,environment_id,computer_id,run_id,attempt_number,$3,id,computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, work.LeaseID, checkpoint, waitID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return waitID, newLease
}

func TestVerificationObservationAPIKeyScope(t *testing.T) {
	f := runtest.New(t)
	var project, environment string
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.slug,e.slug FROM projects p JOIN environments e ON e.project_id=p.id WHERE e.id=$1`, f.EnvironmentID).Scan(&project, &environment); err != nil {
		t.Fatal(err)
	}
	permissions := []string{}
	for _, p := range auth.AllPermissions() {
		permissions = append(permissions, string(p))
	}
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,role,permissions,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'admin',$5,'verification','scope-prefix',$6)`, key, f.OrgID, f.ProjectID, f.EnvironmentID, permissions, dbtest.Hash("observer-key"))
	inputs := map[string]any{"key_prefix": "scope-prefix", "project": project, "environment": environment}
	keys := observe(t, f.Pool, "api-key-scope", inputs).([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["valid"] != true {
		t.Fatalf("authorized key=%v", keys)
	}
	for _, state := range []string{"expired", "revoked", "missing-grant", "wrong-scope"} {
		t.Run(state, func(t *testing.T) {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=NULL,expires_at=NULL,permissions=$2 WHERE id=$1`, key, permissions)
			inputs["environment"] = environment
			switch state {
			case "expired":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET expires_at=now()-interval '1 minute' WHERE id=$1`, key)
			case "revoked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=now() WHERE id=$1`, key)
			case "missing-grant":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET permissions=ARRAY['runs.read'] WHERE id=$1`, key)
			case "wrong-scope":
				inputs["environment"] = "another"
			}
			got := observe(t, f.Pool, "api-key-scope", inputs).([]any)
			if state == "missing-grant" {
				if len(got) != 1 || got[0].(map[string]any)["valid"] != false {
					t.Fatalf("missing permission=%v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("ineligible key=%v", got)
			}
		})
	}
	// A quoted opaque locator is data; it cannot escape the JSON input literal.
	malicious := `host\';DELETE FROM runs;--`
	if got := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{malicious}}).([]any); len(got) != 0 {
		t.Fatalf("quoted resource=%v", got)
	}
}

func TestVerificationObservationLiveAndInitialPaths(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	path := func() map[string]any {
		return observe(t, f.Pool, "run-path", map[string]any{"run_id": work.RunID.String()}).(map[string]any)["leases"].([]any)[0].(map[string]any)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET created_at=now()-interval '1 hour' WHERE run_id=$1`, work.RunID)
	if got := path()["inferred_path"]; got != "cold_instance_allocation" {
		t.Fatalf("cold=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET created_at=now()+interval '1 second' WHERE run_id=$1`, work.RunID)
	if got := path()["inferred_path"]; got != "prepared_instance_claim" {
		t.Fatalf("prepared=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id)
 SELECT $2,environment_id,id,computer_id,'timer',now()+interval '1 minute',revision,current_attempt_number,current_run_lease_id FROM runs WHERE id=$1`, work.RunID, uuid.NewV7())
	if got := path()["inferred_path"]; got != "resident_live_wait" {
		t.Fatalf("hot=%v", got)
	}
}

func TestVerificationObservationCapacityAndFleet(t *testing.T) {
	f := runtest.New(t)
	inputs := map[string]any{"region": runtest.Region, "max_age_seconds": 300}
	for _, populated := range []bool{false, true} {
		if populated {
			f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
		}
		bins := observe(t, f.Pool, "capacity-state", inputs).([]any)
		native, err := db.New(f.Pool).ListWorkerCapacityBins(t.Context(), db.ListWorkerCapacityBinsParams{RegionID: runtest.Region, ObservationFreshnessSeconds: 300, RowLimit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(bins) != 1 || len(native) != 1 {
			t.Fatalf("bins=%v native=%v", bins, native)
		}
		got := bins[0].(map[string]any)
		want := native[0]
		for key, value := range map[string]int64{"available_cpu_millis": want.AvailableCPUMillis, "available_memory_bytes": want.AvailableMemoryBytes, "available_guest_ephemeral_disk_bytes": want.AvailableGuestEphemeralDiskBytes, "available_vm_slots": want.AvailableVMSlots, "available_instance_starts": want.AvailableInstanceStarts} {
			if got[key] != float64(value) {
				t.Fatalf("%s=%v want=%d", key, got[key], value)
			}
		}
	}
	fleet := observe(t, f.Pool, "worker-fleet", map[string]any{"region": runtest.Region}).([]any)
	found := false
	for _, row := range fleet {
		host := row.(map[string]any)
		if host["id"] == f.WorkerID.String() {
			found = true
			if host["vm_platform_id"] != f.VMPlatformID || host["kernel_digest"] == nil || host["arch"] != "x86_64" {
				t.Fatalf("host=%v", host)
			}
		}
	}
	if !found {
		t.Fatalf("host absent from fleet=%v", fleet)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=now()-interval '10 minutes' WHERE id=$1`, f.WorkerID)
	if got := observe(t, f.Pool, "capacity-state", inputs).([]any); len(got) != 0 {
		t.Fatalf("stale bins=%v", got)
	}
	if got := observe(t, f.Pool, "capacity-state", map[string]any{"region": "other-region", "max_age_seconds": 300}).([]any); len(got) != 0 {
		t.Fatalf("foreign region=%v", got)
	}
}

func TestVerificationObservationWholeComputerMembership(t *testing.T) {
	f, first, second, request := computertest.Capture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, run := range []runtest.RunLease{first, second} {
		path := observe(t, f.Pool, "run-path", map[string]any{"run_id": run.RunID.String()}).(map[string]any)
		checkpoints := path["checkpoints"].([]any)
		if len(checkpoints) != 1 {
			t.Fatalf("checkpoints=%v", checkpoints)
		}
		checkpoint := checkpoints[0].(map[string]any)
		if checkpoint["id"] != cp.ID.String() || len(checkpoint["members"].([]any)) != 2 || checkpoint["source_worker_host_id"] != f.WorkerID.String() {
			t.Fatalf("checkpoint=%v", checkpoint)
		}
		members := map[string]bool{}
		for _, value := range checkpoint["members"].([]any) {
			members[value.(map[string]any)["run_id"].(string)] = true
		}
		if !members[first.RunID.String()] || !members[second.RunID.String()] {
			t.Fatalf("members=%v", members)
		}
	}
	// Missing membership cannot be synthesized from sharing a Computer.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET suspension_status='hot',suspend_checkpoint_id=NULL WHERE run_id=$1`, second.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_checkpoint_runs WHERE run_id=$1`, second.RunID)
	path := observe(t, f.Pool, "run-path", map[string]any{"run_id": first.RunID.String()}).(map[string]any)
	if got := path["checkpoints"].([]any)[0].(map[string]any)["members"].([]any); len(got) != 1 {
		t.Fatalf("missing membership fabricated=%v", got)
	}
	other := observe(t, f.Pool, "run-path", map[string]any{"run_id": second.RunID.String()}).(map[string]any)
	if len(other["checkpoints"].([]any)) != 0 {
		t.Fatalf("unproven checkpoint=%v", other)
	}
}
