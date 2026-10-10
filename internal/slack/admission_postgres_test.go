package slack

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

func (f statusFixture) reply(t *testing.T, key string, files bool) uuid.UUID {
	t.Helper()
	payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Thread: "123.456", Text: "<@bot> follow-up", UnsupportedFiles: files})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}
func (f statusFixture) link(t *testing.T) {
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_user_links(team_id,slack_user_id,user_id) VALUES('team','human',$1)`, f.User)
}

func TestSlackBoundReplyCommitsOneCoreTurnAndExactReceipt(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "reply", false)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id)
			if err != nil {
				t.Errorf("admission: %v", err)
			}
		})
	}
	wg.Wait()
	var count int
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate work: %d %v", count, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='accepted' AND r.user_id=$2 AND r.session_id=$3 AND r.thread_source_id=$4 AND r.operation='enqueue' AND convert_from(t.input,'UTF8')::jsonb='[{"type":"text","text":" follow-up"}]'::jsonb FROM slack_requests r JOIN turns t ON t.environment_id=r.environment_id AND t.session_id=r.session_id AND t.id=r.turn_id WHERE r.id=$1`, id, f.User, f.Session, f.participant).Scan(&exact); err != nil || !exact {
		t.Fatalf("receipt or input mismatch: %v %v", exact, err)
	}
}

func TestSlackBoundReplyRejectsCurrentAuthorityAndContentFailures(t *testing.T) {
	for _, kind := range []string{"unlinked", "member-disabled", "publication-revoked", "reauthorized", "expired", "file", "closed", "deleted-root"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			if kind != "unlinked" {
				f.link(t)
			}
			id := f.reply(t, "reply", kind == "file")
			switch kind {
			case "deleted-root":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET deleted_at=clock_timestamp() WHERE id=$1`, f.thread)
			case "member-disabled":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.User)
			case "publication-revoked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
			case "reauthorized":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorized_at=clock_timestamp() WHERE id=$1`, f.installation)
			case "expired":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
			case "closed":
				if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
					t.Fatal(err)
				}
			}
			err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id)
			if err != nil {
				t.Fatalf("admission: %v", err)
			}
			var rejected bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND turn_id IS NULL FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
				t.Fatalf("invalid input not rejected: %v %v", rejected, err)
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected input created work: %d %v", count, err)
			}
		})
	}
}

func TestSlackBoundReplyRollsBackCoreWhenReceiptCannotCommit(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "reply", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests ADD CONSTRAINT reject_acceptance CHECK(status<>'accepted')`)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err == nil {
		t.Fatal("receipt failure missing")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("core escaped failed receipt transaction: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_requests DROP CONSTRAINT reject_acceptance`)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestSlackBoundReplyExpiresWhileCoreAdmissionWaits(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "waiting-reply", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, id)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err = blocker.Exec(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.Environment); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err = blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); result <- err }()
	observed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if observed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !observed {
		t.Fatal("admission did not wait on core lock")
	}
	var expired bool
	for !expired && time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at<=clock_timestamp() FROM slack_requests WHERE id=$1`, id).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if !expired {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !expired {
		t.Fatal("receipt deadline did not elapse")
	}
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='gesture_expired' FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("expired waiting request accepted: %v %v", rejected, err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late Turn survived rollback: %d %v", count, err)
	}
}

func (f statusFixture) newMessage(t *testing.T, key, root string) uuid.UUID {
	t.Helper()
	payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Thread: root, Text: "<@bot> new task"})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}

func TestSlackNewHumanThreadCreatesOneSessionAndBindsLaterReplies(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	root := slackNow()
	first, second := f.newMessage(t, "first", root), f.newMessage(t, "second", root)
	var wg sync.WaitGroup
	for _, id := range []uuid.UUID{first, second, first, second} {
		wg.Go(func() {
			if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
				t.Errorf("admission: %v", err)
			}
		})
	}
	wg.Wait()
	// A concurrent root lookup can consume the read budget before the winning
	// admission commits. The next worker pass uses the now-pinned root.
	for _, id := range []uuid.UUID{first, second} {
		if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
			t.Fatal(err)
		}
	}
	var sessions, turns, threads int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE environment_id=$1),(SELECT count(*) FROM turns WHERE environment_id=$1),(SELECT count(*) FROM slack_threads WHERE thread_ts=$2)`, f.Environment, root).Scan(&sessions, &turns, &threads); err != nil || sessions != 2 || turns != 2 || threads != 1 {
		var reasons string
		_ = f.Pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(status||':'||coalesce(error,''),','),'') FROM slack_requests`).Scan(&reasons)
		t.Fatalf("root duplication: sessions=%d turns=%d threads=%d %v; requests=%s", sessions, turns, threads, err, reasons)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(DISTINCT r.session_id)=1 AND count(*) FILTER(WHERE r.operation='start')=1 AND count(*) FILTER(WHERE r.operation='enqueue')=1 FROM slack_requests r JOIN slack_thread_sources p ON p.id=r.thread_source_id JOIN slack_threads t ON t.id=p.thread_id WHERE t.thread_ts=$1 AND r.status='accepted' AND p.session_id=t.front_session_id`, root).Scan(&exact); err != nil || !exact {
		t.Fatalf("start/reply receipt binding: %v %v", exact, err)
	}
}

func TestSlackNewHumanRootAcceptsText(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	id := f.newMessage(t, "new", "")
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatalf("admission: %v", err)
	}
	var accepted bool
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='accepted' FROM slack_requests WHERE id=$1`, id).Scan(&accepted); err != nil || !accepted {
		t.Fatalf("text start rejected: %v %v", accepted, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 2 {
		t.Fatalf("accepted start missing Session: %d %v", count, err)
	}
}

func TestSlackHistoricalRootCannotBindAcrossPublicationGeneration(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	nextPublication := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) SELECT $2,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id FROM agent_publications WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.publication, nextPublication)
	id := f.newMessage(t, "old-root", "123.456")
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='thread_generation_unavailable' AND NOT EXISTS(SELECT 1 FROM turns) FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
		t.Fatal("historical root rebound", rejected, err)
	}
}

func newSlackAdmissionFixture(t *testing.T) statusFixture {
	t.Helper()
	f := newStatusFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$3::text) WHERE environment_id=$1 AND id=$2;
 UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE computers SET preparation_spec_id=$2,origin_deployment_id=$2,origin_definition_key='fixture-computer',resources='{"milliCpu":1000,"memoryMiB":512}',storage_reservation_bytes=$4 WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, definition.ComputerSeedProfile, disk.SeedCapacity)
	return f
}

func TestSlackNewHumanRootDoesNotWaitForAgentToReturnToDeployment(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	absent := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$2,'sha256:'||repeat('2',64) FROM deployments WHERE environment_id=$1 AND id=$3;
 UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, absent, f.Deployment)
	id := f.newMessage(t, "missing-agent", "")
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatalf("admission: %v", err)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='rejected' AND error='session_unavailable' FROM slack_requests WHERE id=$1`, id).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("missing Agent parked request: %v %v", rejected, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("restored Agent replayed old gesture: %d %v", count, err)
	}
}

func (f statusFixture) selectedMessage(t *testing.T, key string) uuid.UUID {
	t.Helper()
	payload, _ := json.Marshal(messageGesture{Channel: "C1", Timestamp: slackNow(), Thread: "123.456", Text: "<@bot> !reviewer inspect"})
	now := time.Now().UTC()
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}

func TestSlackSelectorTextDoesNotChangeFront(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	id := f.selectedMessage(t, "literal-selector")
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.session_id=$2 AND convert_from(t.input,'UTF8')::jsonb='[{"type":"text","text":" !reviewer inspect"}]'::jsonb FROM slack_requests r JOIN turns t ON t.id=r.turn_id WHERE r.id=$1`, id, f.Session).Scan(&exact); err != nil || !exact {
		t.Fatal(exact, err)
	}
}
