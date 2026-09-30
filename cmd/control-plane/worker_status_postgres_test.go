package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerStatusCommandsUseWorkerGroupOperationsPostgres(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", database.DSN)
	dbtest.MustExec(t, t.Context(), database.Pool, `INSERT INTO regions (id, display_name) VALUES ('local', 'Local')`)
	created, err := workergroup.CreateGroup(t.Context(), database.Pool, workergroup.GroupInput{RegionID: "local", Name: "default"})
	if err != nil {
		t.Fatal(err)
	}
	groupID := uuid.UUID(created.Group.ID.Bytes).String()
	run := func(command func(context.Context, io.Writer, []string) error, args ...string) ([]byte, error) {
		var output bytes.Buffer
		err := command(t.Context(), &output, args)
		return output.Bytes(), err
	}

	output, err := run(runWorkerGroupStatusCommand, "pause", "--group-id", groupID, "--expected-claim-version", strconv.FormatInt(created.Group.ClaimVersion, 10))
	if err != nil {
		t.Fatal(err)
	}
	var paused workergroup.GroupStatus
	if err := json.Unmarshal(output, &paused); err != nil || paused.Status != "paused" || !paused.TransitionApplied {
		t.Fatalf("pause output = %s, err = %v", output, err)
	}
	var conflicting workergroup.ConflictError
	if _, err := run(runWorkerGroupStatusCommand, "activate", "--group-id", groupID, "--expected-claim-version", strconv.FormatInt(created.Group.ClaimVersion, 10)); !errors.As(err, &conflicting) {
		t.Fatalf("stale activate error = %v, want ConflictError", err)
	}
	output, err = run(runWorkerGroupStatusCommand, "status", "--group-id", groupID)
	var status workergroup.GroupStatus
	if err != nil || json.Unmarshal(output, &status) != nil || status.ClaimVersion != paused.ClaimVersion {
		t.Fatalf("status output = %s, err = %v", output, err)
	}
	if _, err := run(runWorkerGroupStatusCommand, "status", "--group-id", uuid.NewV7().String()); !errors.Is(err, workergroup.ErrGroupNotFound) {
		t.Fatalf("missing group status error = %v", err)
	}

	_, pool, err := workergroup.CreatePool(t.Context(), database.Pool, uuid.UUID(created.Group.ID.Bytes), "pending", paused.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), database.Pool, `
INSERT INTO worker_hosts (id, resource_id, worker_group_id, worker_pool_id, status)
VALUES ($1, 'host-1', $2, $3, 'registering')`, uuid.NewV7(), created.Group.ID, pool.ID)
	output, err = run(runWorkerHostStatusCommand, "status", "--group-id", groupID, "--resource-id", "host-1")
	var host workergroup.HostStatus
	if err != nil || json.Unmarshal(output, &host) != nil || host.Status != "registering" {
		t.Fatalf("host status output = %s, err = %v", output, err)
	}
	output, err = run(runWorkerHostStatusCommand, "lose", "--group-id", groupID, "--resource-id", "host-1", "--expected-claim-version", strconv.FormatInt(host.ClaimVersion, 10))
	var lost workergroup.HostStatus
	if err != nil || json.Unmarshal(output, &lost) != nil || lost.Status != "lost" || !lost.TransitionApplied {
		t.Fatalf("lose output = %s, err = %v", output, err)
	}
}
