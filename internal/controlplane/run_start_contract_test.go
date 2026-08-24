package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	startWaitID       = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc41"
	startCheckpointID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc42"
	startAttachID     = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc43"
)

func TestParseRunStartArm(t *testing.T) {
	tests := []struct {
		name    string
		request workerapi.RunStartRequest
		mode    runLeaseClaimMode
		ok      bool
	}{
		{name: "fresh", request: workerapi.RunStartRequest{Fresh: &workerapi.RunStartFresh{}}, mode: runLeaseClaimFresh, ok: true},
		{name: "restore", request: workerapi.RunStartRequest{Restore: &workerapi.RunStartRestore{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID, ResumeRequestVersion: 1,
		}}, mode: runLeaseClaimRestore, ok: true},
		{name: "bad UUID", request: workerapi.RunStartRequest{Restore: &workerapi.RunStartRestore{
			RunWaitID: "bad", CheckpointID: startCheckpointID, ResumeAttachID: startAttachID, ResumeRequestVersion: 1,
		}}},
		{name: "bad version", request: workerapi.RunStartRequest{Restore: &workerapi.RunStartRestore{
			RunWaitID: startWaitID, CheckpointID: startCheckpointID, ResumeAttachID: startAttachID,
		}}},
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
		{name: "same workspace child is fresh", locators: db.GetRunLeaseStartLocatorsRow{ParentRunID: valid, EnclosingWaitID: valid}, mode: runLeaseClaimFresh},
		{name: "same workspace parent restores", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, RunWaitCheckpointID: valid,
			RuntimeRestoreCheckpointID: valid,
		}, mode: runLeaseClaimRestore},
		{name: "nested restore", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid, EnclosingWaitID: valid,
		}, mode: runLeaseClaimRestore},
		{name: "different Workspace child resume", locators: db.GetRunLeaseStartLocatorsRow{
			RunWaitID: valid,
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
