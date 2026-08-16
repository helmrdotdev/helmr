package run

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDecideFreshLeaseLossUsesExactPhysicalReason(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	base := db.GetFreshRunLeaseLossAuthorityRow{
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
		mutate func(*db.GetFreshRunLeaseLossAuthorityRow)
		reason string
	}{
		{name: "worker lost", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) { row.WorkerLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "worker epoch changed", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) {
			row.WorkerCurrentEpoch.Int64 = 2
			row.WorkerEpochStartedAt = timestamp(now)
		}, reason: "worker_lost"},
		{name: "runtime lost", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) { row.RuntimeLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "runtime failed", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) { row.RuntimeFailedAt = timestamp(now) }, reason: "runtime_failed"},
		{name: "mount lost", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) { row.MountLostAt = timestamp(now) }, reason: "worker_lost"},
		{name: "mount failed", mutate: func(row *db.GetFreshRunLeaseLossAuthorityRow) { row.MountFailedAt = timestamp(now) }, reason: "runtime_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base
			tc.mutate(&row)
			loss, found, err := decideFreshLeaseLoss(row)
			if err != nil {
				t.Fatal(err)
			}
			if !found || loss.reason != tc.reason || loss.state != db.RunLeaseStateLost {
				t.Fatalf("loss = %+v, found=%v; want %s/lost", loss, found, tc.reason)
			}
		})
	}
}

func TestDecideFreshLeaseLossUsesEarliestAuthoritativeBoundary(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	row := db.GetFreshRunLeaseLossAuthorityRow{
		RunLeaseState:        string(db.RunLeaseStateAssigned),
		ObservedAt:           timestamp(now),
		RunLeaseExpiresAt:    timestamp(now.Add(-2 * time.Second)),
		StartDeadlineAt:      timestamp(now.Add(-3 * time.Second)),
		WorkerCurrentEpoch:   pgtype.Int8{Int64: 2, Valid: true},
		WorkerEpoch:          1,
		WorkerEpochStartedAt: timestamp(now.Add(-time.Second)),
	}
	loss, found, err := decideFreshLeaseLoss(row)
	if err != nil {
		t.Fatal(err)
	}
	if !found || loss.reason != "lease_expired" ||
		!loss.at.Equal(now.Add(-3*time.Second)) || loss.state != db.RunLeaseStateExpired {
		t.Fatalf("loss = %+v, found=%v", loss, found)
	}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
