package control

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	startWaitID       = "00000000-0000-0000-0000-000000000001"
	startCheckpointID = "00000000-0000-0000-0000-000000000002"
	startAttachID     = "00000000-0000-0000-0000-000000000003"
)

func TestParseRunStartArm(t *testing.T) {
	tests := []struct {
		name    string
		request api.WorkerRunStartRequest
		mode    runLeaseClaimMode
		ok      bool
	}{
		{name: "fresh", request: api.WorkerRunStartRequest{Fresh: &api.WorkerRunStartFresh{}}, mode: runLeaseClaimFresh, ok: true},
		{name: "restore", request: api.WorkerRunStartRequest{Restore: &api.WorkerRunStartRestore{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID, ResumeRequestVersion: 1,
		}}, mode: runLeaseClaimRestore, ok: true},
		{name: "child", request: api.WorkerRunStartRequest{Attach: &api.WorkerRunStartAttach{Child: &api.WorkerRunStartChildAttach{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID,
		}}}, mode: runLeaseClaimAttachChild, ok: true},
		{name: "parent", request: api.WorkerRunStartRequest{Attach: &api.WorkerRunStartAttach{Parent: &api.WorkerRunStartParentAttach{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID, ResumeRequestVersion: 2,
		}}}, mode: runLeaseClaimAttachParent, ok: true},
		{name: "bad UUID", request: api.WorkerRunStartRequest{Restore: &api.WorkerRunStartRestore{
			RunWaitID: "bad", CheckpointID: startCheckpointID, ResumeAttachID: startAttachID, ResumeRequestVersion: 1,
		}}},
		{name: "bad version", request: api.WorkerRunStartRequest{Attach: &api.WorkerRunStartAttach{Parent: &api.WorkerRunStartParentAttach{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID,
		}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			arm, err := parseRunStartArm(test.request)
			if (err == nil) != test.ok {
				t.Fatalf("error = %v", err)
			}
			if test.ok && arm.mode != test.mode {
				t.Fatalf("mode = %q, want %q", arm.mode, test.mode)
			}
		})
	}
}

func TestDeriveRunStartModeFromDurableLocators(t *testing.T) {
	valid := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	tests := []struct {
		name     string
		locators db.GetRunLeaseStartLocatorsRow
		mode     runLeaseClaimMode
	}{
		{name: "fresh", mode: runLeaseClaimFresh},
		{name: "restore", locators: db.GetRunLeaseStartLocatorsRow{RunWaitID: valid}, mode: runLeaseClaimRestore},
		{name: "child", locators: db.GetRunLeaseStartLocatorsRow{ParentRunID: valid, EnclosingWaitID: valid}, mode: runLeaseClaimAttachChild},
		{name: "parent", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, ResumeChildRunID: valid,
			ResumeChildParentOwned: pgtype.Bool{Bool: true, Valid: true},
		}, mode: runLeaseClaimAttachParent},
		{name: "recreated parent", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, RunWaitCheckpointID: valid,
			RuntimeRestoreCheckpointID: valid, ResumeChildRunID: valid,
			ResumeChildParentOwned: pgtype.Bool{Bool: true, Valid: true},
		}, mode: runLeaseClaimRestore},
		{name: "nested restore", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, EnclosingWaitID: valid,
		}, mode: runLeaseClaimRestore},
		{name: "different Workspace child resume", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, ResumeChildRunID: valid,
			ResumeChildParentOwned: pgtype.Bool{Bool: false, Valid: true},
		}, mode: runLeaseClaimRestore},
		{name: "detached child", locators: db.GetRunLeaseStartLocatorsRow{ParentRunID: valid}, mode: runLeaseClaimFresh},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode := deriveRunStartMode(test.locators)
			if mode != test.mode {
				t.Fatalf("mode = %q, want %q", mode, test.mode)
			}
		})
	}
}
