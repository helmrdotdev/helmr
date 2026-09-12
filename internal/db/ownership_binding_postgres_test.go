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
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) SELECT $1,org_id,project_id,environment_id,digest,'workspace_version',size_bytes,media_type FROM artifacts WHERE id=$2", wrong, d.ProgramArtifactID)
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

func ownershipVersionParams(t *testing.T, f runLeaseClaimFixture, work runLeaseWork) CreatePrivateCheckpointWorkspaceVersionParams {
	t.Helper()
	ctx := t.Context()
	p := CreatePrivateCheckpointWorkspaceVersionParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.environmentID), ArtifactID: pgvalue.UUID(uuid.NewV7()), ContentDigest: dbtest.Digest("ownership-version"), SizeBytes: 1, EntryCount: 1}
	if err := f.pool.QueryRow(ctx, "SELECT workspace_id,base_version_id,id,ownership_generation,writer_generation FROM workspace_leases WHERE owner_run_lease_id=$1", work.leaseID).Scan(&p.WorkspaceID, &p.ParentVersionID, &p.SourceWorkspaceLeaseID, &p.OwnershipGeneration, &p.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES ($1,$2,1,'application/octet-stream')", f.orgID, p.ContentDigest)
	dbtest.MustExec(t, ctx, f.pool, "INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,'workspace_version',1,'application/octet-stream')", p.ArtifactID, f.orgID, f.projectID, f.environmentID, p.ContentDigest)
	return p
}

func TestOwnershipDerivedVersionInsertionGates(t *testing.T) {
	for _, publish := range []bool{false, true} {
		name := "checkpoint"
		if publish {
			name = "task and actor publication"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newRunLeaseClaimFixture(t, ctx)
			work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			p := ownershipVersionParams(t, f, work)
			wrong := pgvalue.UUID(uuid.NewV7())
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) SELECT $1,org_id,project_id,environment_id,digest,'deployment_program',size_bytes,media_type FROM artifacts WHERE id=$2", wrong, p.ArtifactID)
			insert := func(q *Queries, p CreatePrivateCheckpointWorkspaceVersionParams) (WorkspaceVersion, error) {
				if !publish {
					return q.CreatePrivateCheckpointWorkspaceVersion(ctx, p)
				}
				return q.PublishTaskWorkspaceVersion(ctx, PublishTaskWorkspaceVersionParams{ID: p.ID, EnvironmentID: p.EnvironmentID, WorkspaceID: p.WorkspaceID, ParentVersionID: p.ParentVersionID, ArtifactID: p.ArtifactID, ContentDigest: p.ContentDigest, SizeBytes: p.SizeBytes, EntryCount: p.EntryCount, SourceWorkspaceLeaseID: p.SourceWorkspaceLeaseID, OwnershipGeneration: p.OwnershipGeneration, WriterGeneration: p.WriterGeneration, PublishedAt: pgvalue.Timestamptz(time.Now())})
			}
			crossScope := ownershipCrossScopeArtifact(t, f, p.ArtifactID)
			for _, id := range []pgtype.UUID{{}, wrong, pgvalue.UUID(uuid.NewV7()), crossScope} {
				bad := p
				bad.ArtifactID = id
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := insert(New(tx), bad); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("invalid artifact=%v err=%v", id, err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				var exists bool
				if err := f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM workspace_versions WHERE id=$1)", p.ID).Scan(&exists); err != nil || exists {
					t.Fatalf("invalid gate inserted row: %v %v", exists, err)
				}
			}
			good, err := insert(f.queries, p)
			if err != nil {
				t.Fatal(err)
			}
			if good.ArtifactID != p.ArtifactID || good.ParentVersionID != p.ParentVersionID {
				t.Fatalf("version=%+v", good)
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			rejectSchemaRow(t, tx, "23503", "UPDATE workspace_versions SET writer_generation=writer_generation+1 WHERE id=$1", p.ID)
		})
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
			startTaskCompletionWork(t, ctx, f, parent)
			claim := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('aa',32),'hex'),decode(repeat('ab',32),'hex'),now())", claim, f.environmentID)
			dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET cause_kind='child',parent_run_id=$2,parent_owns_lifecycle=false,claim_id=$3 WHERE id=$1", child.runID, parent.runID, claim)
			if resolved {
				dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET status='succeeded',terminal_at=now() WHERE id=$1", child.runID)
			}
			var version int64
			var childWorkspace pgtype.UUID
			if err := f.pool.QueryRow(ctx, "SELECT state_version FROM runs WHERE id=$1", parent.runID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(ctx, "SELECT workspace_id FROM runs WHERE id=$1", child.runID).Scan(&childWorkspace); err != nil {
				t.Fatal(err)
			}
			p := RegisterDifferentWorkspaceChildCallParams{ID: pgvalue.UUID(uuid.NewV7()), ChildRunID: pgvalue.UUID(child.runID), ChildTargetDeclaredID: pgvalue.Text("test-task"), ChildClaimID: pgvalue.UUID(claim), ChildRequest: []byte("{}"), RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("child-binding")), AttemptNumber: 1, CurrentRunLeaseID: pgvalue.UUID(parent.leaseID), ResumeAttachID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.environmentID), RunID: pgvalue.UUID(parent.runID), ChildWorkspaceID: childWorkspace, ExpectedRunningStateVersion: version}
			bind := func(q *Queries, p RegisterDifferentWorkspaceChildCallParams) (RunWait, error) {
				if !resolved {
					return q.RegisterDifferentWorkspaceChildCall(ctx, p)
				}
				return q.RegisterResolvedDifferentWorkspaceChildCall(ctx, RegisterResolvedDifferentWorkspaceChildCallParams{ID: p.ID, ChildRunID: p.ChildRunID, ChildTargetDeclaredID: p.ChildTargetDeclaredID, ChildClaimID: p.ChildClaimID, ChildRequest: p.ChildRequest, RegistrationRequestFingerprint: p.RegistrationRequestFingerprint, AttemptNumber: p.AttemptNumber, CurrentRunLeaseID: p.CurrentRunLeaseID, ResumeAttachID: p.ResumeAttachID, EnvironmentID: p.EnvironmentID, RunID: p.RunID, ExpectedRunningStateVersion: p.ExpectedRunningStateVersion, ConditionResult: []byte("{}")})
			}
			snapshot := func() []byte {
				var b []byte
				if err := f.pool.QueryRow(ctx, "SELECT to_jsonb(r) FROM runs r WHERE id=$1", parent.runID).Scan(&b); err != nil {
					t.Fatal(err)
				}
				return b
			}
			for _, test := range []string{"detached", "missing", "wrong parent"} {
				before := snapshot()
				bad := p
				if test == "missing" {
					bad.ChildRunID = pgvalue.UUID(uuid.NewV7())
				}
				if test == "wrong parent" {
					bad.RunID = pgvalue.UUID(child.runID)
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
			dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET parent_owns_lifecycle=true WHERE id=$1", child.runID)
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
