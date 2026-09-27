package db

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestOwnershipDeploymentProgramGateAndReplay(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	d, err := f.queries.GetDeployment(ctx, GetDeploymentParams{OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID), ID: pgvalue.UUID(f.base.DeploymentID)})
	if err != nil {
		t.Fatal(err)
	}
	p := CreateDeploymentParams{ID: pgvalue.UUID(uuid.NewV7()), OrgID: d.OrgID, ProjectID: d.ProjectID, EnvironmentID: d.EnvironmentID, Version: d.Version, BundleDigest: d.BundleDigest, RuntimeArtifactDigest: d.RuntimeArtifactDigest, ProgramArtifactID: d.ProgramArtifactID, ProgramIndexDigest: d.ProgramIndexDigest, QueueConfig: d.QueueConfig}
	wrong := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) SELECT $1,org_id,project_id,environment_id,digest,'computer_disk_version',size_bytes,media_type FROM artifacts WHERE id=$2", wrong, d.ProgramArtifactID)
	crossScope := ownershipCrossScopeArtifact(t, f, d.ProgramArtifactID)
	for _, id := range []pgtype.UUID{{}, pgvalue.UUID(uuid.NewV7()), wrong, crossScope} {
		bad := p
		bad.ProgramArtifactID = id
		for _, replay := range []bool{false, true} {
			if !replay {
				bad.BundleDigest = dbtest.Digest("fresh-invalid")
			}
			if _, err := f.queries.CreateDeployment(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid program replay=%v err=%v", replay, err)
			}
			bad.BundleDigest = p.BundleDigest
		}
	}
	got, err := f.queries.CreateDeployment(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != d.ID || got.CreatedAt != d.CreatedAt || got.ProgramArtifactID != d.ProgramArtifactID {
		t.Fatalf("replay changed deployment %+v", got)
	}
	var count int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM deployments").Scan(&count); err != nil || count != 1 {
		t.Fatalf("deployments=%d err=%v", count, err)
	}
}

func TestOwnershipScheduleWholeBatchGateIncludesExistingFallback(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	task2, actor := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO deployment_definitions(id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest) VALUES ($1,$3,$4,'task','second',0,'{}',decode(repeat('01',32),'hex')),($2,$3,$4,'actor','actor',0,'{}',decode(repeat('02',32),'hex'))", task2, actor, f.environmentID, f.base.DeploymentID)
	var first pgtype.UUID
	if err := f.pool.QueryRow(ctx, "SELECT id FROM deployment_definitions WHERE environment_id=$1 AND kind='task' AND declared_id='test-task'", f.environmentID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	makeParams := func() ReconcileSchedulesParams {
		now := pgvalue.Timestamptz(time.Now().UTC())
		return ReconcileSchedulesParams{EnvironmentID: pgvalue.UUID(f.environmentID), Ids: []pgtype.UUID{pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())}, TaskDeclaredIds: []string{"test-task", "second"}, DeploymentDefinitionIds: []pgtype.UUID{first, pgvalue.UUID(task2)}, DeploymentIds: []pgtype.UUID{pgvalue.UUID(f.base.DeploymentID), pgvalue.UUID(f.base.DeploymentID)}, CronPatterns: []string{"* * * * *", "* * * * *"}, Timezones: []string{"UTC", "UTC"}, EffectiveFroms: []pgtype.Timestamptz{now, now}, NextFireAts: []pgtype.Timestamptz{now, now}, CronSemanticsVersion: "robfig-cron-v3.0.1/standard-5-field"}
	}
	snapshot := func() []byte {
		var b []byte
		if err := f.pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY id),'[]') FROM schedules s").Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, existing := range []bool{false, true} {
		if existing {
			p := makeParams()
			p.Ids = p.Ids[:1]
			p.TaskDeclaredIds = p.TaskDeclaredIds[:1]
			p.DeploymentDefinitionIds = p.DeploymentDefinitionIds[:1]
			p.DeploymentIds = p.DeploymentIds[:1]
			p.CronPatterns = p.CronPatterns[:1]
			p.Timezones = p.Timezones[:1]
			p.EffectiveFroms = p.EffectiveFroms[:1]
			p.NextFireAts = p.NextFireAts[:1]
			if _, err := f.queries.ReconcileSchedules(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name   string
			mutate func(*ReconcileSchedulesParams)
		}{
			{"wrong role", func(p *ReconcileSchedulesParams) {
				p.DeploymentDefinitionIds[1] = pgvalue.UUID(actor)
				p.TaskDeclaredIds[1] = "actor"
			}},
			{"wrong declared", func(p *ReconcileSchedulesParams) { p.TaskDeclaredIds[0] = "other" }},
			{"wrong deployment", func(p *ReconcileSchedulesParams) { p.DeploymentIds[0] = pgvalue.UUID(uuid.NewV7()) }},
			{"null definition", func(p *ReconcileSchedulesParams) { p.DeploymentDefinitionIds[0] = pgtype.UUID{} }},
			{"unequal arrays", func(p *ReconcileSchedulesParams) { p.DeploymentIds = p.DeploymentIds[:1] }},
		} {
			t.Run(test.name, func(t *testing.T) {
				before := snapshot()
				p := makeParams()
				test.mutate(&p)
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				rows, err := New(tx).ReconcileSchedules(ctx, p)
				if err != nil || len(rows) != 0 {
					t.Fatalf("invalid batch returned %d rows err=%v", len(rows), err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, snapshot()) {
					t.Fatal("invalid mixed batch mutated schedules")
				}
			})
		}
	}
	p := makeParams()
	if rows, err := f.queries.ReconcileSchedules(ctx, p); err != nil || len(rows) != 2 {
		t.Fatalf("valid=%v err=%v", rows, err)
	}
	before := snapshot()
	if rows, err := f.queries.ReconcileSchedules(ctx, p); err != nil || len(rows) != 2 {
		t.Fatalf("replay=%v err=%v", rows, err)
	}
	if !bytes.Equal(before, snapshot()) {
		t.Fatal("no-op replay changed schedule generation or timestamps")
	}
	duplicate := makeParams()
	duplicate.TaskDeclaredIds[1] = duplicate.TaskDeclaredIds[0]
	duplicate.DeploymentDefinitionIds[1] = duplicate.DeploymentDefinitionIds[0]
	if rows, err := f.queries.ReconcileSchedules(ctx, duplicate); err != nil || len(rows) != 2 {
		t.Fatalf("unchanged existing duplicate=%v err=%v", rows, err)
	}
	if !bytes.Equal(before, snapshot()) {
		t.Fatal("unchanged existing duplicate mutated schedules")
	}
	dbtest.MustExec(t, ctx, f.pool, "DELETE FROM schedules")
	if _, err := f.queries.ReconcileSchedules(ctx, duplicate); err == nil {
		t.Fatal("duplicate new batch unexpectedly inserted")
	}
	if got := snapshot(); string(got) != "[]" {
		t.Fatalf("duplicate new batch left partial insertion: %s", got)
	}

}

func TestOwnershipChildBindingRejectsDetachedAndWrongParentBeforeMutation(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		name := "pending"
		if resolved {
			name = "resolved"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newRunLeaseClaimFixture(t, ctx)
			parent := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			child := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, parent.runID)
			claim := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('aa',32),'hex'),decode(repeat('ab',32),'hex'),now())", claim, f.environmentID)
			dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET cause_kind='child',parent_run_id=$2,parent_owns_lifecycle=false,claim_id=$3 WHERE id=$1", child.runID, parent.runID, claim)
			if resolved {
				dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET status='succeeded',terminal_at=now() WHERE id=$1", child.runID)
			}
			var version int64
			var childComputer pgtype.UUID
			if err := f.pool.QueryRow(ctx, "SELECT revision FROM runs WHERE id=$1", parent.runID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(ctx, "SELECT computer_id FROM runs WHERE id=$1", child.runID).Scan(&childComputer); err != nil {
				t.Fatal(err)
			}
			p := RegisterChildCallParams{ID: pgvalue.UUID(uuid.NewV7()), ChildRunID: pgvalue.UUID(child.runID), ChildTargetDeclaredID: pgvalue.Text("test-task"), ChildClaimID: pgvalue.UUID(claim), ChildRequest: []byte("{}"), RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("child-binding")), AttemptNumber: 1, CurrentRunLeaseID: pgvalue.UUID(parent.leaseID), EnvironmentID: pgvalue.UUID(f.environmentID), RunID: pgvalue.UUID(parent.runID), ChildComputerID: childComputer, ExpectedRunningRevision: version}
			bind := func(q *Queries, p RegisterChildCallParams) (RunWait, error) {
				if !resolved {
					return q.RegisterChildCall(ctx, p)
				}
				return q.RegisterResolvedChildCall(ctx, RegisterResolvedChildCallParams{ID: p.ID, ChildRunID: p.ChildRunID, ChildTargetDeclaredID: p.ChildTargetDeclaredID, ChildClaimID: p.ChildClaimID, ChildRequest: p.ChildRequest, RegistrationRequestFingerprint: p.RegistrationRequestFingerprint, AttemptNumber: p.AttemptNumber, CurrentRunLeaseID: p.CurrentRunLeaseID, EnvironmentID: p.EnvironmentID, RunID: p.RunID, ExpectedRunningRevision: p.ExpectedRunningRevision, ConditionResult: []byte("{}")})
			}
			snapshot := func() []byte {
				var b []byte
				if err := f.pool.QueryRow(ctx, "SELECT jsonb_build_array(to_jsonb(r),(SELECT coalesce(jsonb_agg(to_jsonb(w)),'[]') FROM run_waits w WHERE w.run_id=r.id)) FROM runs r WHERE id=$1", parent.runID).Scan(&b); err != nil {
					t.Fatal(err)
				}
				return b
			}
			otherParent := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			for _, test := range []string{"detached", "missing", "wrong parent"} {
				parentID := parent.runID
				if test == "wrong parent" {
					parentID = otherParent.runID
				}
				dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET parent_owns_lifecycle=$2,parent_run_id=$3 WHERE id=$1", child.runID, test != "detached", parentID)
				before := snapshot()
				bad := p
				if test == "missing" {
					bad.ChildRunID = pgvalue.UUID(uuid.NewV7())
				}
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := bind(New(tx), bad); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("%s rejection=%v", test, err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, snapshot()) {
					t.Fatalf("%s moved parent", test)
				}
			}
			dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET parent_owns_lifecycle=true,parent_run_id=$2 WHERE id=$1", child.runID, parent.runID)
			if !resolved {
				before := snapshot()
				bad := p
				bad.ChildComputerID = pgvalue.UUID(uuid.NewV7())
				if _, err := bind(f.queries, bad); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("wrong child Computer=%v", err)
				}
				if !bytes.Equal(before, snapshot()) {
					t.Fatal("wrong Computer moved parent")
				}
			}
			got, err := bind(f.queries, p)
			if err != nil {
				t.Fatal(err)
			}
			if got.ChildRunID != p.ChildRunID {
				t.Fatalf("binding=%+v", got)
			}
		})
	}
}

func ownershipCrossScopeArtifact(t *testing.T, f runLeaseClaimFixture, source pgtype.UUID) pgtype.UUID {
	t.Helper()
	ctx := t.Context()
	environment, id := uuid.NewV7(), pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO environments(id,org_id,project_id,slug,name,color_hex) VALUES($1,$2,$3,$4,'Other','#123456')", environment, f.orgID, f.projectID, "other-"+environment.String())
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) SELECT $1,org_id,project_id,$2,digest,kind,size_bytes,media_type FROM artifacts WHERE id=$3", id, environment, source)
	return id
}

func TestResolvedSharedChildCallDoesNotRestoreComputer(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	parent := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, parent.runID)
	claim := uuid.NewV7()
	child := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('bc',32),'hex'),decode(repeat('bd',32),'hex'),now())`, claim, f.environmentID)
	// The completed child shares the parent's Computer; result delivery must not change it.
	dbtest.MustExec(t, ctx, f.pool, `WITH child AS (INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,parent_run_id,parent_owns_lifecycle,computer_id,base_computer_disk_version_id,status,output,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id,claim_id,terminal_at)
SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,'child',id,true,computer_id,base_computer_disk_version_id,'succeeded','{"reportPath":"/computer/report.pdf"}',queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id,$3,now() FROM runs WHERE id=$1 RETURNING *) INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id,terminal_outcome,terminal_reason_code,terminal_at) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id,'succeeded','completed',now() FROM child`, parent.runID, child, claim)
	var revision int64
	var before, after []byte
	if err := f.pool.QueryRow(ctx, `SELECT revision FROM runs WHERE id=$1`, parent.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT to_jsonb(c) FROM computers c JOIN runs r ON r.computer_id=c.id WHERE r.id=$1`, parent.runID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	p := RegisterResolvedChildCallParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.environmentID), RunID: pgvalue.UUID(parent.runID), ChildRunID: pgvalue.UUID(child), ChildClaimID: pgvalue.UUID(claim), ChildTargetDeclaredID: pgvalue.Text("test-task"), ChildRequest: []byte(`{}`), ConditionResult: []byte(`{"ok":true,"output":{"reportPath":"/computer/report.pdf"}}`), RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("shared-result")), ExpectedRunningRevision: revision, AttemptNumber: 1, CurrentRunLeaseID: pgvalue.UUID(parent.leaseID)}
	wait, err := f.queries.RegisterResolvedChildCall(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if wait.SuspensionStatus != RunWaitStatusReleased || wait.SuspendCheckpointID.Valid {
		t.Fatalf("result imposed disk restoration: %+v", wait)
	}
	if err = f.pool.QueryRow(ctx, `SELECT to_jsonb(c) FROM computers c JOIN runs r ON r.computer_id=c.id WHERE r.id=$1`, parent.runID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("result reuse changed Computer authority")
	}
}
