package workergroup

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/region"
)

func TestCreateGroupPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO regions (id, display_name) VALUES ('us-west-2', 'West')`)
	created, err := CreateGroup(t.Context(), f.pool, GroupInput{RegionID: "us-west-2", Name: "second", Description: "  runs  "})
	if err != nil {
		t.Fatal(err)
	}
	if created.Group.Status != db.WorkerGroupStatusActive || created.Group.Description != "runs" || created.Group.ClaimVersion != 1 {
		t.Fatalf("created group = %+v", created.Group)
	}
	if !strings.HasPrefix(created.EnrollmentToken, auth.EnrollmentTokenPrefix) {
		t.Fatalf("enrollment token = %q", created.EnrollmentToken)
	}
	var storedHash []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT token_hash FROM worker_group_tokens WHERE id = $1`, created.Group.TokenID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if string(storedHash) != string(auth.HashCredential(created.EnrollmentToken)) {
		t.Fatal("stored enrollment token hash does not match the returned token")
	}

	var conflicting ConflictError
	for _, duplicate := range []GroupInput{{RegionID: "us-west-2", Name: "second"}, {RegionID: fixtureRegionID, Name: "another-active"}} {
		if _, err := CreateGroup(t.Context(), f.pool, duplicate); !errors.As(err, &conflicting) {
			t.Fatalf("CreateGroup(%+v) error = %v, want ConflictError", duplicate, err)
		}
	}
	if _, err := CreateGroup(t.Context(), f.pool, GroupInput{RegionID: "missing", Name: "third"}); !errors.Is(err, region.ErrNotFound) {
		t.Fatalf("missing region error = %v", err)
	}
	var input InputError
	for _, bad := range []GroupInput{{RegionID: " padded ", Name: "third"}, {RegionID: fixtureRegionID, Name: "Upper"}} {
		if _, err := CreateGroup(t.Context(), f.pool, bad); !errors.As(err, &input) {
			t.Fatalf("CreateGroup(%+v) error = %v, want InputError", bad, err)
		}
	}
}

func TestGroupReadsAndUpdatesPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	groups, err := ListGroups(t.Context(), f.q, fixtureRegionID, 10)
	if err != nil || len(groups) != 1 || groups[0].ID != f.group.ID {
		t.Fatalf("ListGroups = %+v, %v", groups, err)
	}
	if groups, err := ListGroups(t.Context(), f.q, "other", 10); err != nil || len(groups) != 0 {
		t.Fatalf("ListGroups(other) = %+v, %v", groups, err)
	}
	var input InputError
	if _, err := ListGroups(t.Context(), f.q, " other", 10); !errors.As(err, &input) {
		t.Fatalf("ListGroups padded region error = %v", err)
	}
	updated, err := UpdateGroupDescription(t.Context(), f.q, f.groupID(), " described ")
	if err != nil || updated.Description != "described" {
		t.Fatalf("UpdateGroupDescription = %+v, %v", updated, err)
	}
	missing := uuid.NewV7()
	if _, err := GetGroup(t.Context(), f.q, missing); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("GetGroup missing error = %v", err)
	}
	if _, err := UpdateGroupDescription(t.Context(), f.q, missing, ""); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("UpdateGroupDescription missing error = %v", err)
	}
	if _, err := RotateGroupToken(t.Context(), f.q, missing); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("RotateGroupToken missing error = %v", err)
	}
	token, err := RotateGroupToken(t.Context(), f.q, f.groupID())
	if err != nil || !strings.HasPrefix(token, auth.EnrollmentTokenPrefix) {
		t.Fatalf("RotateGroupToken = %q, %v", token, err)
	}
}

func TestGroupTransitionsPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	paused, err := PauseGroup(t.Context(), f.pool, f.groupID(), f.group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != db.WorkerGroupStatusPaused || paused.ClaimVersion != f.group.ClaimVersion+1 || !paused.TransitionApplied {
		t.Fatalf("paused = %+v", paused)
	}
	replay, err := PauseGroup(t.Context(), f.pool, f.groupID(), f.group.ClaimVersion)
	if err != nil || replay.TransitionApplied || replay.ClaimVersion != paused.ClaimVersion {
		t.Fatalf("pause replay = %+v, %v", replay, err)
	}
	var conflicting ConflictError
	if _, err := ActivateGroup(t.Context(), f.pool, f.groupID(), f.group.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("stale activation error = %v, want ConflictError", err)
	}
	activated, err := ActivateGroup(t.Context(), f.pool, f.groupID(), paused.ClaimVersion)
	if err != nil || activated.Status != db.WorkerGroupStatusActive {
		t.Fatalf("activated = %+v, %v", activated, err)
	}
	status, err := ReadGroupStatus(t.Context(), f.q, f.groupID())
	if err != nil || status.ClaimVersion != activated.ClaimVersion || status.Status != db.WorkerGroupStatusActive {
		t.Fatalf("ReadGroupStatus = %+v, %v", status, err)
	}
	if _, err := DisableGroup(t.Context(), f.pool, uuid.NewV7(), 1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("missing group transition error = %v", err)
	}
	if _, err := ReadGroupStatus(t.Context(), f.q, uuid.NewV7()); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("missing group status error = %v", err)
	}
	var input InputError
	if _, err := DisableGroup(t.Context(), f.pool, f.groupID(), 0); !errors.As(err, &input) {
		t.Fatalf("zero claim version error = %v, want InputError", err)
	}
}

func TestGroupDrainClearsPrimaryPoolPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	group, err := f.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: f.group.ID, WorkerPoolID: pool.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if group.PrimaryPoolID != pool.ID || group.ClaimVersion != f.group.ClaimVersion+1 {
		t.Fatalf("initial primary = %+v", group)
	}
	activationReplay, err := f.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: f.group.ID, WorkerPoolID: pool.ID,
	})
	if err != nil || activationReplay.PrimaryPoolID != pool.ID || activationReplay.ClaimVersion != group.ClaimVersion {
		t.Fatalf("initial primary activation replay = %+v, %v", activationReplay, err)
	}
	replacement := f.activePool(t, "replacement")
	afterReplacementSeal, err := f.q.SetInitialWorkerGroupPrimaryPool(t.Context(), db.SetInitialWorkerGroupPrimaryPoolParams{
		WorkerGroupID: f.group.ID, WorkerPoolID: replacement.ID,
	})
	if err != nil || afterReplacementSeal.PrimaryPoolID != pool.ID || afterReplacementSeal.ClaimVersion != group.ClaimVersion {
		t.Fatalf("primary after replacement seal = %+v, %v", afterReplacementSeal, err)
	}

	status, err := BeginGroupDrain(t.Context(), f.pool, f.groupID(), group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != db.WorkerGroupStatusDraining || status.ClaimVersion != group.ClaimVersion+1 || !status.TransitionApplied {
		t.Fatalf("group drain status = %+v", status)
	}
	draining := f.currentGroup(t)
	if draining.Status != db.WorkerGroupStatusDraining || draining.ClaimVersion != group.ClaimVersion+1 || draining.PrimaryPoolID.Valid {
		t.Fatalf("draining group = %+v", draining)
	}
	replay, err := BeginGroupDrain(t.Context(), f.pool, f.groupID(), group.ClaimVersion)
	if err != nil || replay.Status != db.WorkerGroupStatusDraining || replay.ClaimVersion != draining.ClaimVersion || replay.TransitionApplied {
		t.Fatalf("group drain replay = %+v, %v", replay, err)
	}
	_, drained, err := DrainPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, pool.ClaimVersion)
	if err != nil || drained.Status != "draining" || drained.ClaimVersion != pool.ClaimVersion+1 {
		t.Fatalf("drained pool = %+v, %v", drained, err)
	}
}

func TestHostStatusPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	hostID := f.activeHost(t, pool, "host-1")
	status, err := ReadHostStatus(t.Context(), f.q, f.groupID(), " host-1 ")
	if err != nil || status.ID != hostID.String() || status.Status != "active" || status.CurrentEpoch == nil || *status.CurrentEpoch != 1 {
		t.Fatalf("ReadHostStatus = %+v, %v", status, err)
	}
	if _, err := ReadHostStatus(t.Context(), f.q, f.groupID(), "host-2"); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("missing host status error = %v", err)
	}
	var input InputError
	if _, err := ReadHostStatus(t.Context(), f.q, f.groupID(), " "); !errors.As(err, &input) {
		t.Fatalf("empty resource ID error = %v, want InputError", err)
	}
	if _, err := MarkHostLost(t.Context(), f.pool, f.groupID(), "host-1", 0); !errors.As(err, &input) {
		t.Fatalf("zero claim version error = %v, want InputError", err)
	}
	var conflicting ConflictError
	if _, err := MarkHostLost(t.Context(), f.pool, f.groupID(), "host-1", status.ClaimVersion+1); !errors.As(err, &conflicting) {
		t.Fatalf("stale loss error = %v, want ConflictError", err)
	}
	lost, err := MarkHostLost(t.Context(), f.pool, f.groupID(), "host-1", status.ClaimVersion)
	if err != nil || lost.Status != "lost" || lost.ClaimVersion != status.ClaimVersion+1 || !lost.TransitionApplied {
		t.Fatalf("MarkHostLost = %+v, %v", lost, err)
	}
}
