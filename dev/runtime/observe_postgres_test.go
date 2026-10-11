package runtime

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os/exec"
	"strings"
	"testing"
	"uuid"
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
	f := agenttest.New(t)
	placement := func() []any {
		return observe(t, f.Pool, "session-placements", map[string]any{"session_ids": []string{f.Session.String()}}).([]any)
	}
	got := placement()
	if len(got) != 1 || got[0].(map[string]any)["worker_host_id"] != f.Worker.String() {
		t.Fatalf("placements=%v", got)
	}
	workers := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{"host"}}).([]any)
	if len(workers) != 1 || workers[0].(map[string]any)["active_leases"] != float64(1) || workers[0].(map[string]any)["unfenced_preparations"] != float64(0) {
		t.Fatalf("workers=%v", workers)
	}
	state := observe(t, f.Pool, "session-state", map[string]any{"session_id": f.Session.String()}).(map[string]any)
	if state["active_leases"] != float64(1) || state["unfenced_processes"] != float64(1) {
		t.Fatalf("session=%v", state)
	}
	computer := observe(t, f.Pool, "computer-state", map[string]any{"computer_id": f.Computer.String()}).(map[string]any)
	if computer["open_sessions"] != float64(1) || computer["active_leases"] != float64(1) {
		t.Fatalf("computer=%v", computer)
	}
	path := observe(t, f.Pool, "computer-path", map[string]any{"computer_id": f.Computer.String()}).(map[string]any)
	if len(path["leases"].([]any)) != 1 || path["computer"].(map[string]any)["initial_root_digest"] == nil {
		t.Fatalf("path=%v", path)
	}
	if observe(t, f.Pool, "clock", map[string]any{}).(map[string]any)["epoch"].(float64) <= 0 {
		t.Fatal("missing clock")
	}
	deployments := observe(t, f.Pool, "deployment-state", map[string]any{"project": "project", "environment": "test"}).([]any)
	if len(deployments) != 1 || len(deployments[0].(map[string]any)["definitions"].([]any)) != 1 {
		t.Fatalf("deployments=%v", deployments)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=now()-interval '1 second'`)
	if got = placement(); len(got) != 0 {
		t.Fatalf("expired placement=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=now()+interval '1 hour'; UPDATE worker_hosts SET current_epoch=current_epoch+1`)
	if got = placement(); len(got) != 0 {
		t.Fatalf("stale placement=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=current_epoch-1; UPDATE session_processes SET fenced_at=now(),status='stopped'`)
	if got = placement(); len(got) != 0 {
		t.Fatalf("fenced process=%v", got)
	}
}

func TestVerificationObservationCurrentHost(t *testing.T) {
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,lost_at) SELECT $2,resource_id,worker_group_id,worker_pool_id,'lost',now() FROM worker_hosts WHERE id=$1`, f.Worker, uuid.NewV7())
	got := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{"host"}}).([]any)
	if len(got) != 1 || got[0].(map[string]any)["id"] != f.Worker.String() {
		t.Fatalf("hosts=%v", got)
	}
}

func TestVerificationScheduleObservation(t *testing.T) {
	f := agenttest.New(t)
	schedule, other, turn := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_schedules(environment_id,id,agent_id,deployment_id,trigger_key,cron,timezone,input,active_from,next_fire_at,lateness_tolerance_ms)
 VALUES($1,$2,$4,$5,'observed','* * * * *','UTC','{}',now(),now(),300000),($1,$3,$4,$5,'other','* * * * *','UTC','{}',now(),now(),300000);
 INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,request_digest,input)
 VALUES($1,$6,$7,$8,1,'schedule',$2,'schedule',$4,decode(repeat('00',32),'hex'),'{}');
 INSERT INTO agent_schedule_occurrences(environment_id,schedule_id,agent_id,trigger_key,scheduled_at,session_id,turn_id,disposition,evaluated_at)
 VALUES($1,$2,$4,'observed',now(),$7,$6,'admitted',now());
 INSERT INTO agent_schedule_occurrences(environment_id,schedule_id,agent_id,trigger_key,scheduled_at,disposition,reason,evaluated_at)
 VALUES($1,$3,$4,'other',now(),'missed','late',now())`, pgx.QueryExecModeSimpleProtocol, f.Environment, schedule, other, f.Agent, f.Deployment, turn, f.Session, f.Computer)
	read := func() map[string]any {
		return observe(t, f.Pool, "schedule-path", map[string]any{"schedule_id": schedule.String()}).(map[string]any)
	}
	path := read()
	rows := path["occurrences"].([]any)
	if len(rows) != 1 || path["has_more"] != false {
		t.Fatalf("schedule path=%v", path)
	}
	row := rows[0].(map[string]any)
	if row["session_id"] != f.Session.String() || row["turn_id"] != turn.String() || row["computer_id"] != f.Computer.String() || row["deployment_id"] != f.Deployment.String() || row["turn_status"] != "queued" {
		t.Fatalf("occurrence ownership=%v", row)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_schedules SET active_until=now(),next_fire_at=now()+interval '1 minute' WHERE id=$1`, schedule)
	if read()["schedule"].(map[string]any)["interval_settled"] != true {
		t.Fatal("closed schedule interval remains pending")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_schedule_occurrences(environment_id,schedule_id,agent_id,trigger_key,scheduled_at,disposition,reason,evaluated_at)
 SELECT $1,$2,$3,'observed',now()+n*interval '1 minute','missed','late',now() FROM generate_series(1,256) n`, f.Environment, schedule, f.Agent)
	path = read()
	if path["has_more"] != true || len(path["occurrences"].([]any)) != 256 {
		t.Fatalf("unbounded schedule path=%v", path)
	}
}

func TestVerificationObservationRejectsQueryInputs(t *testing.T) {
	for _, args := range [][]string{{"../persistence", `{}`}, {"session-state", `{"session_id":"x';DELETE FROM sessions;--"}`}, {"clock", `{"sql":"DELETE FROM sessions"}`}, {"session-placements", `{"session_ids":[]}`}, {"worker-state", `{"resource_ids":["host\nnewline"]}`}, {"run-state", `{}`}} {
		out, err := exec.CommandContext(t.Context(), "python3", append([]string{"observe.py"}, args...)...).CombinedOutput()
		if err == nil || strings.Contains(string(out), "BEGIN READ ONLY;") {
			t.Fatalf("invalid inputs rendered SQL: %q %s", args, out)
		}
	}
}

func TestVerificationObservationAPIKeyScope(t *testing.T) {
	f := agenttest.New(t)
	var project, environment string
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.slug,e.slug FROM projects p JOIN environments e ON e.project_id=p.id WHERE e.id=$1`, f.Environment).Scan(&project, &environment); err != nil {
		t.Fatal(err)
	}
	permissions := []string{}
	for _, p := range auth.AllPermissions() {
		permissions = append(permissions, string(p))
	}
	var org, projectID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &projectID); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,role,permissions,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'admin',$5,'verification','scope-prefix',$6)`, key, org, projectID, f.Environment, permissions, dbtest.Hash("observer-key"))
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
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET permissions=ARRAY['sessions.read'] WHERE id=$1`, key)
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
	malicious := `host\';DELETE FROM sessions;--`
	if got := observe(t, f.Pool, "worker-state", map[string]any{"resource_ids": []string{malicious}}).([]any); len(got) != 0 {
		t.Fatalf("quoted resource=%v", got)
	}
}

func TestVerificationObservationCapacityAndFleet(t *testing.T) {
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=now()`)
	inputs := map[string]any{"region": "test", "max_age_seconds": 300}
	// Host epoch changes alone do not release either physical reservation owner.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=2,epoch_cpu_millis=4000,epoch_memory_bytes=4294967296,epoch_guest_ephemeral_disk_bytes=4294967296,max_vm_slots=4,max_vm_starts=4`)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest) SELECT environment_id,$1,$2,'observer','running',now()+interval '1 hour',1,worker_host_id,worker_epoch,$3,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest FROM computer_leases`, uuid.NewV7(), f.Deployment, uuid.NewV7())
	for _, fenced := range []bool{false, true} {
		if fenced {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='released',fenced_at=now(),fence_evidence='observed shutdown'; UPDATE computer_preparations SET status='failed',error_code='cancelled',fenced_at=now(),fence_evidence='observed shutdown'`)
		}
		bins := observe(t, f.Pool, "capacity-state", inputs).([]any)
		native, err := db.New(f.Pool).ListWorkerCapacityBins(t.Context(), db.ListWorkerCapacityBinsParams{RegionID: "test", ObservationFreshnessSeconds: 300, RowLimit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(bins) != 1 || len(native) != 1 {
			t.Fatalf("bins=%v native=%v", bins, native)
		}
		got := bins[0].(map[string]any)
		wantSlots := float64(2)
		if fenced {
			wantSlots = 4
		}
		if got["available_vm_slots"] != wantSlots {
			t.Fatalf("physical reservations=%v", got)
		}
		want := native[0]
		for key, value := range map[string]int64{"available_cpu_millis": want.AvailableCPUMillis, "available_memory_bytes": want.AvailableMemoryBytes, "available_guest_ephemeral_disk_bytes": want.AvailableGuestEphemeralDiskBytes, "available_vm_slots": want.AvailableVMSlots, "available_instance_starts": want.AvailableInstanceStarts} {
			if got[key] != float64(value) {
				t.Fatalf("%s=%v want=%d", key, got[key], value)
			}
		}
	}
	fleet := observe(t, f.Pool, "worker-fleet", map[string]any{"region": "test"}).([]any)
	if len(fleet) != 1 || fleet[0].(map[string]any)["arch"] != "x86_64" || fleet[0].(map[string]any)["kernel_digest"] == nil {
		t.Fatalf("fleet=%v", fleet)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=now()-interval '10 minutes'`)
	if got := observe(t, f.Pool, "capacity-state", inputs).([]any); len(got) != 0 {
		t.Fatalf("stale=%v", got)
	}
	if got := observe(t, f.Pool, "capacity-state", map[string]any{"region": "other", "max_age_seconds": 300}).([]any); len(got) != 0 {
		t.Fatalf("foreign=%v", got)
	}
}

func TestVerificationObservationCheckpointMembership(t *testing.T) {
	f := agenttest.New(t)
	save, checkpoint, peer := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest) VALUES($1,$2,$3,1,1,$4,'capturing','request',now()+interval '1 hour',decode(repeat('01',32),'hex'))`, f.Environment, checkpoint, f.Computer, save)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_members(environment_id,checkpoint_id,session_id,process_epoch,computer_id) VALUES($1,$2,$3,1,$4)`, f.Environment, checkpoint, f.Session, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) SELECT history_retention_mode,environment_id,$2,agent_id,deployment_id,computer_id,$2,0 FROM sessions WHERE id=$1`, f.Session, peer)
	sessionPath := func(id uuid.UUID) map[string]any {
		return observe(t, f.Pool, "session-path", map[string]any{"session_id": id.String()}).(map[string]any)
	}
	if got := sessionPath(f.Session); len(got["checkpoints"].([]any)) != 1 || len(got["processes"].([]any)) != 1 {
		t.Fatalf("member=%v", got)
	}
	if got := sessionPath(peer); len(got["checkpoints"].([]any)) != 0 {
		t.Fatalf("nonmember=%v", got)
	}
	for _, role := range []string{"vm_config", "vm_state", "memory", "scratch_disk"} {
		digest := "sha256:" + strings.Repeat("2", 64)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,1) ON CONFLICT DO NOTHING`, digest)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_objects(environment_id,checkpoint_id,role,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,1,'application/octet-stream')`, f.Environment, checkpoint, role, digest)
	}
	path := observe(t, f.Pool, "computer-path", map[string]any{"computer_id": f.Computer.String()}).(map[string]any)
	cp := path["checkpoints"].([]any)[0].(map[string]any)
	if len(cp["members"].([]any)) != 1 || len(cp["artifacts"].([]any)) != 4 || cp["target_lease_epoch"] != nil || cp["ready_at"] != nil {
		t.Fatalf("checkpoint=%v", cp)
	}
	if len(path["saves"].([]any)) != 1 {
		t.Fatalf("saves=%v", path)
	}

	// A second resident process is visible only after its exact membership is recorded.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$3,1,'ready')`, f.Environment, peer, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_checkpoint_members(environment_id,checkpoint_id,session_id,process_epoch,computer_id) VALUES($1,$2,$3,1,$4)`, f.Environment, checkpoint, peer, f.Computer)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_saves SET status='published',flush_acknowledged_at=requested_at,captured_at=requested_at,captured_root_digest=c.initial_root_digest,root_id=c.initial_root_id,capture_evidence='captured',publication_evidence='published' FROM computers c WHERE computer_saves.computer_id=c.id AND computer_saves.environment_id=c.environment_id`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='released',fenced_at=now(),fence_evidence='source stopped'`)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at,restored_from_save_id) SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,now()+interval '1 hour','active',$1,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,now(),now(),$2 FROM computer_leases WHERE epoch=1`, uuid.NewV7(), save)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='consumed',manifest='manifest',vm_platform_id=(SELECT vm_platform_id FROM computer_leases WHERE epoch=2),ready_at=now(),target_lease_epoch=2,restore_control_version=2,controls_reconciled_at=now(),capture_request=NULL WHERE id=$1`, checkpoint)
	path = observe(t, f.Pool, "computer-path", map[string]any{"computer_id": f.Computer.String()}).(map[string]any)
	cp = path["checkpoints"].([]any)[0].(map[string]any)
	leases := path["leases"].([]any)
	if len(cp["members"].([]any)) != 2 || cp["status"] != "consumed" || cp["source_lease_epoch"] != float64(1) || cp["target_lease_epoch"] != float64(2) || cp["controls_reconciled_at"] == nil || cp["ready_at"] == nil {
		t.Fatalf("recorded restore=%v", cp)
	}
	if len(leases) != 2 || leases[0].(map[string]any)["fenced_at"] == nil || leases[1].(map[string]any)["restored_from_save_id"] != save.String() {
		t.Fatalf("restore leases=%v", leases)
	}
	for _, id := range []uuid.UUID{f.Session, peer} {
		if got := sessionPath(id)["checkpoints"].([]any); len(got) != 1 || got[0].(map[string]any)["process_epoch"] != float64(1) {
			t.Fatalf("member lineage=%v", got)
		}
	}
	command := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(environment_id,id,computer_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,ARRAY['true'],'{}','',1000,'user','fixture')`, f.Environment, command, f.Computer)
	commands := func() []any {
		return observe(t, f.Pool, "computer-path", map[string]any{"computer_id": f.Computer.String()}).(map[string]any)["commands"].([]any)
	}
	if got := commands(); len(got) != 1 || got[0].(map[string]any)["computer_lease_epoch"] != nil {
		t.Fatalf("pending command=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='running',computer_lease_epoch=2,started_at=now() WHERE id=$1`, command)
	if got := commands()[0].(map[string]any); got["computer_lease_epoch"] != float64(2) || got["status"] != "running" {
		t.Fatalf("bound command=%v", got)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_checkpoint_members WHERE checkpoint_id=$1`, checkpoint)
	if got := sessionPath(f.Session); len(got["checkpoints"].([]any)) != 0 {
		t.Fatalf("removed member=%v", got)
	}
}

func TestSlackDeliveryObservationExcludesPrivatePayloads(t *testing.T) {
	f := agenttest.New(t)
	installation, channel, publication, thread, participant, registration := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO slack_app_registrations(id,organization_id,created_by_user_id) SELECT $11,org_id,$3 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$11,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['chat:write'],$3 FROM environments WHERE id=$1;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($5,$1,$6,$11,$2,$3);
 INSERT INTO slack_channels(id,installation_id,slack_channel_id,environment_id,publication_id,organization_id,team_id) SELECT $4,$2,'C1',$1,$5,org_id,'team' FROM environments WHERE id=$1;
 UPDATE sessions SET slack_channel_id=$4 WHERE environment_id=$1 AND id=$7;
 INSERT INTO slack_threads(id,channel_id,environment_id,front_session_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key,stream_status_repair,delivery_error)
 SELECT $8,$4,$1,$7,org_id,'team','C1','123.456','opening',true,'slack_delivery_warning' FROM environments WHERE id=$1;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($9,$8,$1,$7);
 INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,publication_key,role,payload,payload_digest,presentation_path)
 VALUES($10,$1,$7,$9,$8,1,'lifecycle','lifecycle',convert_to('PRIVATE_PAYLOAD','UTF8'),decode(repeat('11',32),'hex'),'post')`, pgx.QueryExecModeSimpleProtocol, f.Environment, installation, f.User, channel, publication, f.Agent, f.Session, thread, participant, uuid.NewV7(), registration)
	value := observe(t, f.Pool, "slack-delivery", map[string]any{"session_id": f.Session.String()}).(map[string]any)
	if value["participant"].(map[string]any)["projected_event_seq"] != float64(0) || value["thread"].(map[string]any)["stream_status_repair"] != true || len(value["posts"].([]any)) != 1 {
		t.Fatalf("missing delivery state: %v", value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"PRIVATE_PAYLOAD", "credential_ciphertext", "credential_nonce", "inflight_payload", "confirmed_stream_text"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private field leaked: %s", private)
		}
	}
	missing := observe(t, f.Pool, "slack-delivery", map[string]any{"session_id": uuid.NewV7().String()}).(map[string]any)
	if missing["thread"] != nil || missing["installation"] != nil || len(missing["posts"].([]any)) != 0 {
		t.Fatalf("cross-session state: %v", missing)
	}
}
