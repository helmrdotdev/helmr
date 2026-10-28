package slack

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestSlackWakeRetriesUnavailableCapacityWithoutCommittingControls(t *testing.T) {
	for _, transport := range []string{"message", "button"} {
		t.Run(transport, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.link(t)
			// Seed a sleeping Computer with its source host gone. This exercises
			// admission against provider headroom, not checkpoint capture or restore.
			save, checkpoint := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_saves(environment_id,computer_id,id,computer_lease_epoch,seq,status,requested_at,flush_acknowledged_at,captured_at,captured_root_digest,capture_evidence,root_id,publication_evidence,published_at)
    SELECT environment_id,id,$3,1,1,'published',statement_timestamp(),statement_timestamp(),statement_timestamp(),initial_root_digest,'fixture',initial_root_id,'fixture',clock_timestamp() FROM computers WHERE environment_id=$1 AND id=$2;
    INSERT INTO checkpoints(environment_id,computer_id,id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,manifest,vm_platform_id,ready_at,capture_digest)
    SELECT environment_id,computer_id,$4,1,1,$3,'ready',convert_to('{}','UTF8'),clock_timestamp()+interval '1 minute',convert_to('{}','UTF8'),vm_platform_id,clock_timestamp(),decode(repeat('11',32),'hex') FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
    UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$5;
    UPDATE worker_groups SET capacity_observed_at=clock_timestamp()-interval '1 minute' WHERE id=$6`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Computer, save, checkpoint, f.Worker, f.Group)
			var id uuid.UUID
			if transport == "message" {
				id = f.threadMessage(t, "wake-capacity", "!wake", time.Now())
			} else {
				id = f.controlReceipt(t, f.control("wake"), nil)
			}
			admit := func() error {
				if transport == "button" {
					return admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, id)
				}
				return admitMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, f.admissionClient(t), id)
			}
			if err := admit(); !errors.Is(err, workergroup.ErrCapacityUnavailable) {
				t.Fatalf("unavailable capacity: %v", err)
			}
			var unchanged bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='received' AND r.error IS NULL AND r.finished_at IS NULL
    AND NOT EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$2)
    AND NOT EXISTS(SELECT 1 FROM session_controls WHERE environment_id=$2)
    AND (SELECT execution_deadline_at IS NULL FROM computers WHERE environment_id=$2 AND id=$3)
    AND (SELECT status='ready' FROM checkpoints WHERE environment_id=$2 AND id=$4)
    FROM slack_requests r WHERE id=$1`, id, f.Environment, f.Computer, checkpoint).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("failed wake changed state: %v %v", unchanged, err)
			}
			sessiontest.ObserveProvisioning(t, f.Fixture)
			for range 2 {
				if err := admit(); err != nil {
					t.Fatal(err)
				}
			}
			var accepted bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='accepted'
    AND (SELECT count(*)=1 AND bool_and(kind='wake' AND status='accepted') FROM computer_controls WHERE environment_id=$2)
    AND (SELECT execution_deadline_at>clock_timestamp() FROM computers WHERE environment_id=$2 AND id=$3)
    FROM slack_requests r WHERE id=$1`, id, f.Environment, f.Computer).Scan(&accepted); err != nil || !accepted {
				t.Fatalf("wake retry did not accept exactly one control: %v %v", accepted, err)
			}
		})
	}
}
