package main

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSecretRevocationBatchStopsCommandsWithinLimit(t *testing.T) {
	f := agenttest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	store, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Create(t.Context(), f.Environment, "COMMAND_TOKEN", []byte("test"), "create-secret")
	if err != nil {
		t.Fatal(err)
	}
	secretID := uuid.UUID(s.ID.Bytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secret_bindings(environment_id,computer_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.Environment, f.Computer, secretID)
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	ids := []uuid.UUID{}
	for _, key := range []string{"first", "second", "third"} {
		c, err := command.Create(t.Context(), f.Pool, command.CreateRequest{OrgID: org, ProjectID: project, EnvironmentID: f.Environment, ComputerID: f.Computer, Creator: command.Creator{SubjectType: "session", SubjectID: f.User.String()}, Argv: []string{"true"}, IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		result, err := command.Claim(t.Context(), f.Pool, host, command.ClaimRequest{EnvironmentID: f.Environment, InstanceID: f.Computer, WriterGeneration: 1, ActiveCommandIDs: ids})
		if err != nil || result.Start == nil || result.Start.Command.ID != c.ID {
			t.Fatalf("claim=%+v: %v", result, err)
		}
		ids = append(ids, uuid.UUID(c.ID.Bytes))
	}
	if _, err = store.Revoke(t.Context(), f.Environment, secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	reconcile := reconcileSecretRevocation(f.Pool)
	for _, tc := range []struct {
		limit          int32
		want, stopping int
	}{{1, 1, 1}, {2, 2, 3}, {2, 0, 3}} {
		n, err := reconcile(t.Context(), f.Environment, secretID, 1, tc.limit)
		if err != nil || n != tc.want {
			t.Fatalf("batch=%d want %d: %v", n, tc.want, err)
		}
		var count int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE status='stopping' AND terminal_at IS NULL AND process_reconciled_at IS NULL`).Scan(&count); err != nil || count != tc.stopping {
			t.Fatalf("stopping=%d want %d: %v", count, tc.stopping, err)
		}
	}
}
