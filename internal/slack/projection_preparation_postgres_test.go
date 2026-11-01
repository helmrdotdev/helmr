package slack

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

func (f statusFixture) queueTurn(t *testing.T, key string) uuid.UUID {
	t.Helper()
	receipt, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: key, Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.TurnID
}

// drainProjection runs discovery as the dispatcher does, so state that changes
// without a Session event must still be found.
func (f statusFixture) drainProjection(t *testing.T) {
	t.Helper()
	for range 30 {
		n, err := ReconcileProjection(t.Context(), f.Pool, testProjectionConfig(), 64)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("projection never drained")
}

func (f statusFixture) preparationCard(t *testing.T, turn uuid.UUID) (title, status string) {
	t.Helper()
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_id=$1 AND role='preparation' AND turn_id=$2`, f.thread, turn).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Blocks []struct {
			Type   string `json:"type"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Blocks) != 1 || body.Blocks[0].Type != "task_card" {
		t.Fatalf("preparation card: %s %v", raw, err)
	}
	return body.Blocks[0].Title, body.Blocks[0].Status
}

func (f statusFixture) sleepComputer(t *testing.T) {
	t.Helper()
	save, checkpoint := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_saves(environment_id,computer_id,id,computer_lease_epoch,seq,status,requested_at,flush_acknowledged_at,captured_at,captured_root_digest,capture_evidence,root_id,publication_evidence,published_at)
 SELECT environment_id,id,$3,1,1,'published',statement_timestamp(),statement_timestamp(),statement_timestamp(),initial_root_digest,'fixture',initial_root_id,'fixture',clock_timestamp() FROM computers WHERE environment_id=$1 AND id=$2;
 INSERT INTO checkpoints(environment_id,computer_id,id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,manifest,vm_platform_id,ready_at,capture_digest)
 SELECT environment_id,computer_id,$4,1,1,$3,'ready',convert_to('{}','UTF8'),clock_timestamp()+interval '1 minute',convert_to('{}','UTF8'),vm_platform_id,clock_timestamp(),decode(repeat('11',32),'hex') FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Computer, save, checkpoint)
}

func TestSlackTurnOnAwakeComputerHasNoPreparationCard(t *testing.T) {
	f := newStatusFixture(t)
	f.queueTurn(t, "warm")
	f.drainProjection(t)
	var cards int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND role='preparation'`, f.thread).Scan(&cards); err != nil || cards != 0 {
		t.Fatalf("cards=%d %v", cards, err)
	}
}

func TestSlackPreparationCardFollowsWakeUntilTheTurnStarts(t *testing.T) {
	f := newStatusFixture(t)
	f.sleepComputer(t)
	turn := f.queueTurn(t, "wake")
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Waking the Computer" || status != "in_progress" {
		t.Fatalf("waiting card: %s %s", title, status)
	}
	f.assertAggregate(t, "processing")
	// An awake Computer alone does not settle the card; the Turn must start.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='active' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Waking the Computer" || status != "in_progress" {
		t.Fatalf("card settled before the Turn started: %s %s", title, status)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='running',process_epoch=1,started_at=clock_timestamp() WHERE id=$1`, turn)
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Computer ready" || status != "complete" {
		t.Fatalf("ready card: %s %s", title, status)
	}
}

func TestSlackPreparationCardShowsAStartingComputerAndItsStop(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	turn := f.queueTurn(t, "start")
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Starting the Computer" || status != "in_progress" {
		t.Fatalf("starting card: %s %s", title, status)
	}
	// The Computer comes up but the Turn is cancelled before it starts.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='active' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	f.drainProjection(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='cancelled',terminal_at=clock_timestamp() WHERE id=$1`, turn)
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Stopped" || status != "error" {
		t.Fatalf("stopped card: %s %s", title, status)
	}
}

func TestSlackStoragePressureWaitIsProcessingWithACard(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$2,$3,'storage_pressure','system')`, f.Environment, uuid.NewV7(), f.Session)
	turn := f.queueTurn(t, "storage")
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Waiting for storage space" || status != "in_progress" {
		t.Fatalf("storage card: %s %s", title, status)
	}
	f.assertAggregate(t, "processing")
}

func TestSlackPreparationCardReportsAComputerThatDidNotStart(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	turn := f.queueTurn(t, "failed-start")
	f.drainProjection(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='failed',terminal_at=clock_timestamp() WHERE id=$1`, turn)
	f.drainProjection(t)
	if title, status := f.preparationCard(t, turn); title != "Could not prepare the Computer" || status != "error" {
		t.Fatalf("failed start card: %s %s", title, status)
	}
}

func TestSlackHeldTurnHasNoPreparationCard(t *testing.T) {
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2;
 INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) VALUES($1,$3,$4,'process_failed','system')`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Computer, uuid.NewV7(), f.Session)
	f.queueTurn(t, "held")
	f.drainProjection(t)
	var cards int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND role='preparation'`, f.thread).Scan(&cards); err != nil || cards != 0 {
		t.Fatalf("held Turn shows preparation: %d %v", cards, err)
	}
}
