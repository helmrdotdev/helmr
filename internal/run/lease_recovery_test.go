package run

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDecideExecutionLeaseLossUsesExactPhysicalReason(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	base := db.GetRunExecutionLeaseLossAuthorityRow{
		RunLeaseState:       string(db.RunLeaseStateRunning),
		ObservedAt:          timestamp(now),
		RunLeaseExpiresAt:   timestamp(now.Add(time.Minute)),
		StartDeadlineAt:     timestamp(now.Add(time.Minute)),
		ActiveStartedAt:     timestamp(now.Add(-time.Second)),
		MaxActiveDurationMs: 60_000,
		WorkerCurrentEpoch:  pgtype.Int8{Int64: 1, Valid: true},
		WorkerEpoch:         1,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*db.GetRunExecutionLeaseLossAuthorityRow)
		reason string
	}{
		{name: "worker lost", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) { row.WorkerLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "worker epoch changed", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) {
			row.WorkerCurrentEpoch.Int64 = 2
			row.WorkerEpochStartedAt = timestamp(now)
		}, reason: "worker_lost"},
		{name: "runtime lost", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) { row.RuntimeLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "runtime failed", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) { row.RuntimeFailedAt = timestamp(now) }, reason: "runtime_failed"},
		{name: "mount lost", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) { row.MountLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "mount failed", mutate: func(row *db.GetRunExecutionLeaseLossAuthorityRow) { row.MountFailedAt = timestamp(now) }, reason: "runtime_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base
			tc.mutate(&row)
			loss, found, err := decideExecutionLeaseLoss(row)
			if err != nil {
				t.Fatal(err)
			}
			if !found || loss.reason != tc.reason || loss.state != db.RunLeaseStateLost {
				t.Fatalf("loss = %+v, found=%v; want %s/lost", loss, found, tc.reason)
			}
		})
	}
}

func TestDecideExecutionLeaseLossUsesEarliestAuthoritativeBoundary(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	row := db.GetRunExecutionLeaseLossAuthorityRow{
		RunLeaseState:        string(db.RunLeaseStateAssigned),
		ObservedAt:           timestamp(now),
		RunLeaseExpiresAt:    timestamp(now.Add(-2 * time.Second)),
		StartDeadlineAt:      timestamp(now.Add(-3 * time.Second)),
		WorkerCurrentEpoch:   pgtype.Int8{Int64: 2, Valid: true},
		WorkerEpoch:          1,
		WorkerEpochStartedAt: timestamp(now.Add(-time.Second)),
	}
	loss, found, err := decideExecutionLeaseLoss(row)
	if err != nil {
		t.Fatal(err)
	}
	if !found || loss.reason != "lease_expired" ||
		!loss.at.Equal(now.Add(-3*time.Second)) || loss.state != db.RunLeaseStateExpired {
		t.Fatalf("loss = %+v, found=%v", loss, found)
	}
}

func TestDecideExecutionLeaseLossStateDeadlines(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	base := db.GetRunExecutionLeaseLossAuthorityRow{
		ObservedAt:          timestamp(now),
		RunLeaseExpiresAt:   timestamp(now.Add(time.Minute)),
		StartDeadlineAt:     timestamp(now.Add(-time.Hour)),
		ActiveStartedAt:     timestamp(now.Add(-10 * time.Second)),
		MaxActiveDurationMs: 5_000,
		WorkerCurrentEpoch:  pgtype.Int8{Int64: 1, Valid: true},
		WorkerEpoch:         1,
	}

	checkpointing := base
	checkpointing.RunLeaseState = string(db.RunLeaseStateCheckpointing)
	loss, found, err := decideExecutionLeaseLoss(checkpointing)
	if err != nil {
		t.Fatal(err)
	}
	if !found || loss.kind != "active_deadline" ||
		loss.reason != "max_active_duration_exceeded" || loss.state != db.RunLeaseStateExpired {
		t.Fatalf("checkpointing loss = %+v, found=%v", loss, found)
	}

	finalizing := base
	finalizing.RunLeaseState = string(db.RunLeaseStateFinalizing)
	finalizing.ActiveStartedAt = pgtype.Timestamptz{}
	finalizing.WorkerLostAt = timestamp(now)
	loss, found, err = decideExecutionLeaseLoss(finalizing)
	if err != nil {
		t.Fatal(err)
	}
	if !found || loss.kind != "physical_loss" || loss.reason != "worker_lost" ||
		!loss.at.Equal(now) {
		t.Fatalf("finalizing loss = %+v, found=%v", loss, found)
	}
}

func TestRuntimeCleanupMountFinalizationUsesTopologyNotReason(t *testing.T) {
	kind, reason := runtimeCleanupMountFinalization(
		"obsolete_reason_must_not_drive_cleanup",
		false,
	)
	if kind.Valid || reason.Valid {
		t.Fatalf("non-discard cleanup = %v/%v, want no finalization", kind, reason)
	}

	kind, reason = runtimeCleanupMountFinalization(
		"max_active_duration_exceeded",
		true,
	)
	if !kind.Valid || kind.String != "discard" ||
		!reason.Valid || reason.String != "max_active_duration_exceeded" {
		t.Fatalf("discard cleanup = %v/%v, want discard with exact reason", kind, reason)
	}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
