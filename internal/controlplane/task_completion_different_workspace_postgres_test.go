package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDifferentWorkspaceChildCompletesAfterBeginFinalization(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "")
}

func TestDifferentWorkspaceChildCompletesAfterSavedHeadAdvancement(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "saved")
}

func TestDifferentWorkspaceChildRetriesAfterSavedHeadAdvancement(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "saved retry")
}

func TestDifferentWorkspaceChildCompletionRefreshesDrainedWorkerClaims(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "worker")
}

func TestDifferentWorkspaceChildCompletionRefreshesGroupClaims(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "group")
}
func TestDifferentWorkspaceChildCompletionRejectsRevokedCredentialAfterDrain(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "revoked")
}
func TestDifferentWorkspaceChildCompletesBetweenParentAttempts(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "parent retry")
}

func TestTaskCompletionCancelsSkippedOwnedChild(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "owned terminal")
}
func TestTaskRetryPreservesSkippedOwnedChild(t *testing.T) {
	testDifferentWorkspaceChildCompletion(t, "owned retry")
}

func testDifferentWorkspaceChildCompletion(t *testing.T, transition string) {
	t.Helper()
	base := runtest.New(t)
	parent := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	child := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	ctx := t.Context()
	var parentWorkspace, parentVersion, parentWorkspaceLease uuid.UUID
	err := base.Pool.QueryRow(ctx, `SELECT r.workspace_id, r.base_workspace_version_id, wl.id
 FROM runs r JOIN workspace_leases wl ON wl.owner_run_lease_id = r.current_run_lease_id WHERE r.id = $1`, parent.RunID).Scan(&parentWorkspace, &parentVersion, &parentWorkspaceLease)
	if err != nil {
		t.Fatal(err)
	}
	var childWorkspace, childVersion, runtimeID, mountID, workspaceLeaseID uuid.UUID
	err = base.Pool.QueryRow(ctx, `SELECT r.workspace_id, r.base_workspace_version_id, l.runtime_instance_id, wl.workspace_mount_id, wl.id
 FROM runs r JOIN run_leases l ON l.id = r.current_run_lease_id JOIN workspace_leases wl ON wl.owner_run_lease_id = l.id WHERE r.id = $1`, child.RunID).Scan(&childWorkspace, &childVersion, &runtimeID, &mountID, &workspaceLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claimID, waitID, checkpointID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	tx, err := base.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO idempotency_claims (id, environment_id, operation, slot_hash, request_fingerprint, accepted_at)
 VALUES ($1,$2,'task.child.invoke',decode(repeat('52',32),'hex'),decode(repeat('54',32),'hex'),transaction_timestamp())`, claimID, base.EnvironmentID)
	dbtest.MustExec(t, ctx, tx, `UPDATE runs SET cause_kind='child', parent_run_id=$2, parent_owns_lifecycle=true, claim_id=$3,
 status='running', started_at=transaction_timestamp()-interval '1 second', active_started_at=transaction_timestamp()-interval '1 second', revision=3 WHERE id=$1`, child.RunID, parent.RunID, claimID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='running', started_at=transaction_timestamp()-interval '1 second' WHERE id=$1`, child.LeaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_attempts SET entrypoint_entered_at=transaction_timestamp()-interval '1 second' WHERE run_id=$1`, child.RunID)
	dbtest.MustExec(t, ctx, tx, `UPDATE runs SET status='waiting', current_run_lease_id=NULL, revision=5 WHERE id=$1`, parent.RunID)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='checkpointed', started_at=claimed_at, checkpointed_at=transaction_timestamp(), terminal_at=transaction_timestamp(), terminal_reason_code='checkpointed' WHERE id=$1`, parent.LeaseID)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_leases SET status='released', released_at=transaction_timestamp(), terminal_at=transaction_timestamp() WHERE id=$1`, parentWorkspaceLease)
	dbtest.MustExec(t, ctx, tx, `UPDATE workspace_mounts SET status='unmounted', unmounted_at=transaction_timestamp(), terminal_at=transaction_timestamp(), terminal_reason_code='checkpointed' WHERE workspace_id=$1`, parentWorkspace)
	dbtest.MustExec(t, ctx, tx, `UPDATE runtime_instances SET desired_state='closed', observed_state='closed', desired_version=2, observed_version=2, observed_desired_version=2, terminal_at=transaction_timestamp(), reclaimed_at=transaction_timestamp(), terminal_reason_code='desired_state_reconciled', reclaim_evidence='{}'::jsonb WHERE workspace_id=$1`, parentWorkspace)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_waits (id, environment_id, run_id, workspace_id, kind, condition_status, child_run_id,
 child_target_declared_id, child_claim_id, child_request, expected_run_revision, attempt_number, prior_run_lease_id,
 checkpoint_request_version, checkpoint_ack_version, resume_attach_id, suspension_status)
 VALUES ($1,$2,$3,$4,'child','pending',$5,'test-task',$6,'{"Method":"call"}'::jsonb,5,1,$7,1,1,$8,'parked')`, waitID, base.EnvironmentID, parent.RunID, parentWorkspace, child.RunID, claimID, parent.LeaseID, uuid.NewV7())
	artifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, parent.RunID, checkpointID.String())
	dbtest.MustExec(t, ctx, tx, `INSERT INTO run_checkpoints (id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id,private_workspace_version_id,runtime_config_artifact_id,vm_state_artifact_id,memory_artifact_id,scratch_disk_artifact_id,status,restore_manifest,ready_request_fingerprint,ready_at)
 VALUES ($1,$2,1,$3,$4,$5,$6,$7,$7,$8,$9,$10,$11,'ready','{"kind":"suspend"}'::jsonb,'sha256:c6a8f322cea284f70d8d5bdfa780132e389aca57ace69073ac76e8daa12dacc8',transaction_timestamp())`, checkpointID, parent.RunID, waitID, parent.LeaseID, parentWorkspaceLease, parentWorkspace, parentVersion, artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
	dbtest.MustExec(t, ctx, tx, `UPDATE run_waits SET suspend_checkpoint_id=$2 WHERE id=$1`, waitID, checkpointID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if transition == "parent retry" {
		tx, err := base.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, ctx, tx, `UPDATE run_waits SET condition_status='failed',condition_reason_code='execution_lost',condition_error='{"code":"execution_lost"}',condition_terminal_at=now(),suspension_status='failed',suspension_reason_code='execution_lost',suspension_terminal_at=now() WHERE id=$1`, waitID)
		dbtest.MustExec(t, ctx, tx, `UPDATE run_checkpoints SET status='invalid',invalidated_at=now(),invalidation_reason_code='execution_lost' WHERE id=$1`, checkpointID)
		dbtest.MustExec(t, ctx, tx, `UPDATE run_attempts SET terminal_outcome='failed',terminal_reason_code='execution_lost',terminal_at=now() WHERE run_id=$1 AND number=1`, parent.RunID)
		dbtest.MustExec(t, ctx, tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,workspace_id,base_workspace_version_id) SELECT id,2,'task',workspace_id,base_workspace_version_id FROM runs WHERE id=$1`, parent.RunID)
		dbtest.MustExec(t, ctx, tx, `UPDATE runs SET status='retry_delayed',current_attempt_number=2,retry_at=now()+interval '1 minute',revision=revision+1 WHERE id=$1`, parent.RunID)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var owned uuid.UUID
	if transition == "owned terminal" || transition == "owned retry" {
		owned = base.AddRunLease(t, "starting", time.Now().Add(-time.Minute)).RunID
		ownedClaim := uuid.NewV7()
		dbtest.MustExec(t, ctx, base.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('21',32),'hex'),decode(repeat('23',32),'hex'),now())`, ownedClaim, base.EnvironmentID)
		dbtest.MustExec(t, ctx, base.Pool, `UPDATE runs SET parent_run_id=$2,parent_owns_lifecycle=true,cause_kind='child',claim_id=$3 WHERE id=$1`, owned, child.RunID, ownedClaim)
	}
	server := &Server{db: db.New(base.Pool), tx: base.Pool, cas: finalizationTestCAS(t)}
	worker := workerActor{WorkerInstanceID: base.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
	assignment := workerapi.RunLeaseAssignment{ID: child.LeaseID.String(), RunID: child.RunID.String(), AttemptNumber: 1, LeaseSequence: 1, WorkerGroupID: runtest.WorkerGroup, WorkerInstanceID: base.WorkerID.String(), WorkerEpoch: 1, RuntimeInstanceID: runtimeID.String(), RuntimeIdentityID: base.RuntimeIdentityID, WorkspaceID: childWorkspace.String(), WorkspaceMountID: mountID.String(), WorkspaceLeaseID: workspaceLeaseID.String(), BaseWorkspaceVersionID: childVersion.String(), OwnershipGeneration: 1, WriterGeneration: 1, MountFencingGeneration: 2}
	begin := workerapi.BeginRunFinalizationRequest{Lease: assignment.Fence(), OperationID: uuid.NewV7().String(), Kind: workerapi.RunFinalizationCapture, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: child.RunID.String(), AttemptNumber: 1, RunLeaseID: child.LeaseID.String()}}
	parsedBegin, err := parseRunFinalization(begin)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := server.beginRunFinalization(ctx, worker, begin, parsedBegin)
	if err != nil {
		t.Fatalf("begin finalization: %v", err)
	}
	assignment.ExpiresAt = frozen.ExpiresAt
	capture := validTaskWorkspaceCapture(t, assignment)
	capture.Receipt.OperationID = frozen.OperationID
	setCaptureFingerprint(t, capture)
	registerFinalizationTestDisk(t, base.Pool, server, worker, assignment.Fence(), capture, frozen.OperationID)
	var savedHead uuid.UUID
	if transition == "saved" || transition == "saved retry" {
		// Model two prior committed saves while retaining the immutable execution
		// fence. Their storage locator is already certified by the capture fixture.
		raw, err := json.Marshal(capture.Disk.Root)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			savedHead = uuid.NewV7()
			dbtest.MustExec(t, ctx, base.Pool, `INSERT INTO computer_versions(id,environment_id,workspace_id,parent_version_id,content_digest,size_bytes,status,source_workspace_lease_id,ownership_generation,writer_generation,published_at)
 SELECT $2,c.environment_id,c.id,c.head_version_id,$4,$5,'committed',$3,c.ownership_generation,c.writer_generation,now() FROM computers c WHERE c.id=$1`, childWorkspace, savedHead, workspaceLeaseID, capture.Disk.Root.Pack.Digest, capture.Disk.Root.LogicalBytes)
			dbtest.MustExec(t, ctx, base.Pool, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator) SELECT environment_id,id,$2,$3 FROM computers WHERE id=$1`, childWorkspace, savedHead, raw)
			dbtest.MustExec(t, ctx, base.Pool, `UPDATE computers SET head_version_id=$2,revision=revision+1 WHERE id=$1`, childWorkspace, savedHead)
		}
	}
	request := workerapi.CompleteTaskRequest{Lease: assignment.Fence(), Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{Output: json.RawMessage(`{"ok":true}`)}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
	if transition == "saved retry" || transition == "owned retry" {
		dbtest.MustExec(t, ctx, base.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'::jsonb WHERE id=$1`, child.RunID)
		request.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "retry me"}}
	}
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if transition == "" {
		for _, test := range []struct {
			name   string
			change func(*workerapi.WorkspaceFinalizationFence)
		}{
			{"epoch", func(f *workerapi.WorkspaceFinalizationFence) { f.WorkerEpoch++ }},
			{"runtime", func(f *workerapi.WorkspaceFinalizationFence) { f.RuntimeInstanceID = uuid.NewV7().String() }},
			{"mount", func(f *workerapi.WorkspaceFinalizationFence) { f.MountFencingGeneration++ }},
			{"workspace", func(f *workerapi.WorkspaceFinalizationFence) { f.WriterGeneration++ }},
			{"sequence", func(f *workerapi.WorkspaceFinalizationFence) { f.LeaseSequence++ }},
			{"expiry", func(f *workerapi.WorkspaceFinalizationFence) { f.ExpiresAt = time.Now().Add(-time.Minute) }},
		} {
			t.Run(test.name, func(t *testing.T) {
				badCapture := *capture
				test.change(&badCapture.Receipt.Fence)
				setCaptureFingerprint(t, &badCapture)
				badRequest := request
				badRequest.Workspace.Captured = &badCapture
				badRequest.Lease.LeaseSequence = badCapture.Receipt.Fence.LeaseSequence
				encoded, err := json.Marshal(badRequest)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, "/worker/v1/run/tasks/complete", bytes.NewReader(encoded)).WithContext(context.WithValue(ctx, workerContextKey{}, worker))
				response := httptest.NewRecorder()
				server.workerCompleteTask(response, r)
				if response.Code != http.StatusConflict {
					t.Fatalf("invalid %s fence status=%d body=%s", test.name, response.Code, response.Body.String())
				}
				var status, condition string
				if err := base.Pool.QueryRow(ctx, `SELECT r.status,w.condition_status FROM runs r JOIN run_waits w ON w.child_run_id=r.id WHERE r.id=$1`, child.RunID).Scan(&status, &condition); err != nil {
					t.Fatal(err)
				}
				if status != "running" || condition != "pending" {
					t.Fatalf("stale completion mutated state: run=%s wait=%s", status, condition)
				}
			})
		}
	}
	if transition != "" && transition != "saved" && transition != "saved retry" {
		keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
		if err != nil {
			t.Fatal(err)
		}
		secret := "test-worker-secret"
		hash, err := auth.HashToken(keys.WorkerInstance, secret)
		if err != nil {
			t.Fatal(err)
		}
		credentialID, serviceID := uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, ctx, base.Pool, `UPDATE worker_instances SET current_service_id=$2 WHERE id=$1`, base.WorkerID, serviceID)
		dbtest.MustExec(t, ctx, base.Pool, `INSERT INTO worker_instance_credentials (id,worker_group_id,worker_instance_id,key_prefix,secret_hash) VALUES ($1,$2,$3,'test-worker',$4)`, credentialID, runtest.WorkerGroupID, base.WorkerID, hash)
		server.authKeys = keys
		server.workerTokenSigningKey = bytes.Repeat([]byte{2}, auth.RootKeySize)
		server.workerTokenTTL = time.Hour
		server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
		drain := func(ctx context.Context) error {
			if transition == "group" {
				_, err := base.Pool.Exec(ctx, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, runtest.WorkerGroupID)
				return err
			}
			_, err := db.New(base.Pool).DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
				ID: pgvalue.UUID(base.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID),
				ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1,
			})
			if err == nil && transition == "revoked" {
				_, err = base.Pool.Exec(ctx, `UPDATE worker_instance_credentials SET revoked_at=now() WHERE id=$1`, credentialID)
			}
			return err
		}
		var requestMu sync.Mutex
		var tokenRequests int
		var receiptBodies [][]byte
		var statuses []int
		complete := server.requireWorker(http.HandlerFunc(server.workerCompleteTask))
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestMu.Lock()
			defer requestMu.Unlock()
			if r.URL.Path == "/worker/v1/instance/token" {
				tokenRequests++
				server.workerAuthToken(w, r)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			receiptBodies = append(receiptBodies, body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			// Invalidate the issued Worker token before the first authenticated completion.
			if len(statuses) == 0 {
				if err := drain(r.Context()); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
			response := httptest.NewRecorder()
			complete.ServeHTTP(response, r)
			statuses = append(statuses, response.Code)
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
		}))
		defer httpServer.Close()
		client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(base.WorkerID.String(), secret), workerclient.WithService(serviceID.String()))
		if err != nil {
			t.Fatal(err)
		}
		err = client.CompleteTask(ctx, request)
		requestMu.Lock()
		defer requestMu.Unlock()
		if transition == "revoked" {
			if !httpclient.IsStatus(err, http.StatusUnauthorized) || tokenRequests != 2 || len(statuses) != 1 || statuses[0] != http.StatusUnauthorized {
				t.Fatalf("revoked credential completion: err=%v token requests=%d statuses=%v", err, tokenRequests, statuses)
			}
			var status, condition, leaseStatus string
			if err := base.Pool.QueryRow(ctx, `SELECT r.status,w.condition_status,l.status FROM runs r JOIN run_waits w ON w.child_run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, child.RunID).Scan(&status, &condition, &leaseStatus); err != nil {
				t.Fatal(err)
			}
			if status != "running" || condition != "pending" || leaseStatus != "finalizing" {
				t.Fatalf("unauthorized completion mutated state: run=%s wait=%s lease=%s", status, condition, leaseStatus)
			}
			return
		}
		if err != nil {
			t.Fatalf("complete through Worker client: %v; statuses=%v", err, statuses)
		}
		if tokenRequests != 2 || len(statuses) != 2 || statuses[0] != http.StatusUnauthorized || statuses[1] != http.StatusNoContent {
			t.Fatalf("token requests=%d completion statuses=%v", tokenRequests, statuses)
		}
		if !bytes.Equal(receiptBodies[0], receiptBodies[1]) {
			t.Fatal("completion receipt changed during authentication replay")
		}
		if transition == "group" {
			worker.GroupClaimVersion++
		} else {
			worker.ClaimVersion++
		}
	}
	if err := server.completeTask(ctx, worker, request, completion); err != nil {
		point, _ := staleAuthorityPointOf(err)
		t.Fatalf("complete task at %s: %v", point, err)
	}
	if owned != uuid.Nil() {
		var status string
		if err := base.Pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, owned).Scan(&status); err != nil {
			t.Fatal(err)
		}
		want := "cancelled"
		if transition == "owned retry" {
			want = "queued"
		}
		if status != want {
			t.Fatalf("owned lifecycle on %s: %s", transition, status)
		}
		return
	}
	if transition == "parent retry" {
		var childState, parentState, waitState string
		if err := base.Pool.QueryRow(ctx, `SELECT c.status,p.status,w.condition_status FROM runs c JOIN runs p ON p.id=c.parent_run_id JOIN run_waits w ON w.id=$2 WHERE c.id=$1`, child.RunID, waitID).Scan(&childState, &parentState, &waitState); err != nil {
			t.Fatal(err)
		}
		if childState != "succeeded" || parentState != "retry_delayed" || waitState != "failed" {
			t.Fatalf("late completion woke old parent: %s/%s/%s", childState, parentState, waitState)
		}
		return
	}
	if transition == "saved" || transition == "saved retry" {
		var origin, parent uuid.UUID
		if err := base.Pool.QueryRow(ctx, `SELECT a.base_workspace_version_id,v.parent_version_id FROM run_attempts a JOIN computers c ON c.id=a.workspace_id JOIN computer_versions v ON v.id=c.head_version_id WHERE a.run_id=$1 AND a.number=1`, child.RunID).Scan(&origin, &parent); err != nil {
			t.Fatal(err)
		}
		if origin != childVersion || parent != savedHead {
			t.Fatalf("wrong execution origin or publication predecessor: %s/%s", origin, parent)
		}
	}
	if transition == "saved retry" {
		var status, condition string
		var attempt int32
		var nextBase, head uuid.UUID
		if err := base.Pool.QueryRow(ctx, `SELECT r.status,w.condition_status,r.current_attempt_number,a.base_workspace_version_id,c.head_version_id FROM runs r JOIN run_waits w ON w.child_run_id=r.id JOIN run_attempts a ON a.run_id=r.id AND a.number=r.current_attempt_number JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, child.RunID).Scan(&status, &condition, &attempt, &nextBase, &head); err != nil {
			t.Fatal(err)
		}
		if status != "retry_delayed" || condition != "pending" || attempt != 2 || nextBase != head || head == savedHead {
			t.Fatalf("retry lost saved frontier: %s/%s attempt=%d base=%s head=%s", status, condition, attempt, nextBase, head)
		}
		return
	}
	var status, condition string
	if err := base.Pool.QueryRow(ctx, `SELECT r.status,w.condition_status FROM runs r JOIN run_waits w ON w.child_run_id=r.id WHERE r.id=$1`, child.RunID).Scan(&status, &condition); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || condition != "completed" {
		t.Fatalf("child status=%s parent wait=%s", status, condition)
	}
	// A completed child carries the quiesced worker finalization receipt even
	// while its separate Workspace keeps its Runtime warm. Parent stop must not
	// require reclaiming that already-proved execution a second time.
	excluded, err := server.db.SessionOwnedExecutionsExcluded(ctx, pgvalue.UUID(parent.RunID))
	if err != nil || !excluded {
		t.Fatalf("completed child blocks parent interruption: %v %v", excluded, err)
	}
}
