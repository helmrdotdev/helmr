package runtime

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"uuid"
)

func TestVerificationObservationPreparationPath(t *testing.T) {
	f := sessiontest.New(t)
	first, second, firstAllocation, secondAllocation := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	replacement, otherEnvironment := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET resource_id='i-historical',status='lost',lost_at=now() WHERE id=$1`, f.Worker)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest)
 SELECT $2,'i-replacement',worker_group_id,worker_pool_id,'active',2,$2,now(),now(),epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest FROM worker_hosts WHERE id=$1`, f.Worker, replacement)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,error_code,deadline_at,executor_epoch,worker_host_id,worker_epoch,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,executor_expires_at,started_at,fenced_at,fence_evidence)
 SELECT environment_id,$1,$2,'first','failed','cancelled','2026-10-27T12:00:00Z',1,worker_host_id,worker_epoch,$3,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,'2026-10-27T10:00:00Z','2026-10-27T11:00:00Z','2026-10-27T10:01:00Z','2026-10-27T10:02:00Z','test physical stop' FROM computer_leases`, first, f.Deployment, firstAllocation)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,predecessor_build_id,idempotency_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT environment_id,$1,image_spec_id,id,'second','running',deadline_at,1,$2,2,$3,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest FROM image_builds WHERE id=$4`, second, replacement, secondAllocation, first)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at)
 SELECT $2,org_id,project_id,'other','Other',color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at FROM environments WHERE id=$1`, f.Environment, otherEnvironment)
	read := func(environment, build uuid.UUID) map[string]any {
		return observe(t, f.Pool, "preparation-path", map[string]any{"environment_id": environment.String(), "image_build_id": build.String()}).(map[string]any)
	}
	path := read(f.Environment, first)
	if _, err := time.Parse(time.RFC3339Nano, path["observed_at"].(string)); err != nil {
		t.Fatalf("observation time=%v: %v", path, err)
	}
	preparation := path["preparation"].(map[string]any)
	for key, want := range map[string]any{
		"environment_id": f.Environment.String(), "image_build_id": first.String(),
		"image_spec_id": f.Deployment.String(), "spec_digest": "sha256:" + strings.Repeat("1", 64),
		"worker_host_id": f.Worker.String(), "worker_epoch": float64(1), "executor_epoch": float64(1),
		"allocation_id": firstAllocation.String(), "status": "failed", "error_code": "cancelled",
		"recorded_host_resource_id": "i-historical", "recorded_host_status": "lost", "predecessor_build_id": nil,
	} {
		if preparation[key] != want {
			t.Fatalf("first %s=%v want=%v", key, preparation[key], want)
		}
	}
	for key, want := range map[string]string{"deadline_at": "2026-10-27T12:00:00Z", "executor_expires_at": "2026-10-27T11:00:00Z", "delivered_at": "2026-10-27T10:00:00Z", "started_at": "2026-10-27T10:01:00Z", "fenced_at": "2026-10-27T10:02:00Z"} {
		got, err := time.Parse(time.RFC3339Nano, preparation[key].(string))
		if err != nil || got.UTC().Format(time.RFC3339) != want {
			t.Fatalf("first %s=%v want=%v: %v", key, preparation[key], want, err)
		}
	}
	// Explicit projection excludes channel credentials, CA keys and opaque payloads.
	if len(preparation) != 18 {
		t.Fatalf("unexpected preparation fields=%v", preparation)
	}
	definitions := path["referencing_definitions"].([]any)
	if len(definitions) != 1 || path["definitions_have_more"] != false {
		t.Fatalf("definitions=%v", path)
	}
	definition := definitions[0].(map[string]any)
	if len(definition) != 4 || definition["deployment_id"] != f.Deployment.String() || definition["template_id"] != f.Deployment.String() || definition["bundle_digest"] != preparation["spec_digest"] || definition["template_key"] != "fixture-computer" {
		t.Fatalf("linkage=%v", definition)
	}
	// A second preparation for the same spec retains its own allocation and Host.
	current := read(f.Environment, second)["preparation"].(map[string]any)
	for key, want := range map[string]any{"image_build_id": second.String(), "predecessor_build_id": first.String(), "allocation_id": secondAllocation.String(), "worker_host_id": replacement.String(), "worker_epoch": float64(2), "recorded_host_resource_id": "i-replacement", "recorded_host_status": "active", "status": "running", "delivered_at": nil, "started_at": nil, "fenced_at": nil} {
		if current[key] != want {
			t.Fatalf("second %s=%v want=%v", key, current[key], want)
		}
	}
	for _, inputs := range [][2]uuid.UUID{{otherEnvironment, first}, {f.Environment, uuid.NewV7()}} {
		missing := read(inputs[0], inputs[1])
		if missing["preparation"] != nil || len(missing["referencing_definitions"].([]any)) != 0 || missing["definitions_have_more"] != false {
			t.Fatalf("foreign or missing build=%v", missing)
		}
	}
	// All referencing Deployments are historical facts, with a bounded output.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployments(environment_id,id,bundle_digest)
 SELECT $1,gen_random_uuid(),'sha256:'||lpad(to_hex(n),64,'0') FROM generate_series(2,258) n`, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,network_policy,prepare_network_policy)
 SELECT d.environment_id,d.id,t.template_id,t.image_spec_id,t.cpu_millis,t.memory_bytes,t.network_policy,t.prepare_network_policy FROM deployments d JOIN template_definitions t ON t.environment_id=d.environment_id AND t.deployment_id=$2 WHERE d.environment_id=$1 AND d.id<>$2`, f.Environment, f.Deployment)
	bounded := read(f.Environment, first)
	if len(bounded["referencing_definitions"].([]any)) != 256 || bounded["definitions_have_more"] != true {
		t.Fatalf("unbounded definitions=%v", bounded)
	}
}

func TestVerificationObservationPreparationInputs(t *testing.T) {
	id := uuid.NewV7().String()
	for _, inputs := range []map[string]any{
		{}, {"environment_id": id}, {"image_build_id": id},
		{"environment_id": id, "image_build_id": id, "sql": "SELECT 1"},
		{"environment_id": []string{id}, "image_build_id": id},
		{"environment_id": id, "image_build_id": []string{id}},
		{"environment_id": "x';DELETE FROM image_builds;--", "image_build_id": id},
		{"environment_id": id, "image_build_id": "invalid"},
	} {
		encoded, _ := json.Marshal(inputs)
		out, err := exec.CommandContext(t.Context(), "python3", "observe.py", "preparation-path", string(encoded)).CombinedOutput()
		if err == nil || strings.Contains(string(out), "BEGIN READ ONLY;") {
			t.Fatalf("invalid inputs rendered SQL: %s %s", encoded, out)
		}
	}
	encoded, _ := json.Marshal(map[string]any{"environment_id": id, "image_build_id": id})
	out, err := exec.CommandContext(t.Context(), "python3", "observe.py", "preparation-path", string(encoded)).CombinedOutput()
	if err != nil {
		t.Fatalf("valid input: %v %s", err, out)
	}
	for _, limit := range []string{"BEGIN READ ONLY;", "SET LOCAL statement_timeout='5s';", "SET LOCAL lock_timeout='1s';", "SET LOCAL idle_in_transaction_session_timeout='10s';", "COMMIT;"} {
		if !strings.Contains(string(out), limit) {
			t.Fatalf("missing read-only bound %q", limit)
		}
	}
}
