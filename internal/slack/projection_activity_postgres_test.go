package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func activityExecution(f statusFixture) session.Execution {
	return session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
}
func activityHost(f statusFixture) workergroup.HostPrincipal {
	return workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
}
func activityChild(t *testing.T, f statusFixture, running bool) (statusFixture, session.Admission) {
	t.Helper()
	return activityChildOrigin(t, f, running, true)
}
func activityChildOrigin(t *testing.T, f statusFixture, running, explicitOrigin bool) (statusFixture, session.Admission) {
	t.Helper()
	var parentTurn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&parentTurn); err != nil {
		t.Fatal(err)
	}
	if !explicitOrigin {
		parentTurn = uuid.UUID{}
	}
	host := activityHost(f)
	caller := session.Caller{Kind: "session", MCPScopeID: f.MCPScope, ID: f.Session, TurnID: parentTurn, Execution: activityExecution(f), Host: &host}
	receipt, err := session.Spawn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, caller, session.StartRequest{EnvironmentID: f.Environment, Agent: "agent", ComputerID: f.Computer, IdempotencyKey: uuid.NewV7().String(), Input: json.RawMessage(`[{"type":"text","text":"PRIVATE CHILD ASSIGNMENT"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	child := f
	child.Session = receipt.SessionID
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_thread_sources WHERE session_id=$1`, child.Session).Scan(&child.participant); err != nil {
		t.Fatal(err)
	}
	if running {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,attachment_sequence,control_sequence,control_attachment,control_kind,control_generation,control_acknowledged_at) VALUES($1,$2,1,$3,1,'ready',1,1,1,'resume',1,clock_timestamp())`, f.Environment, child.Session, f.Computer)
		if _, err = dispatchRuntimeTurn(t.Context(), f.Pool, activityExecution(child)); err != nil {
			t.Fatal(err)
		}
	}
	return child, receipt
}
func projectAllActivity(t *testing.T, f statusFixture) {
	t.Helper()
	for range 256 {
		n, err := ReconcileProjection(t.Context(), f.Pool, testProjectionConfig(), 64)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("activity projection stalled")
}

type activityBody struct {
	Text   string `json:"text"`
	Blocks []struct {
		Type     string `json:"type"`
		TaskID   string `json:"task_id"`
		BlockID  string `json:"block_id"`
		Status   string `json:"status"`
		Elements []struct {
			Action string `json:"action_id"`
			Value  string `json:"value"`
		} `json:"elements"`
	} `json:"blocks"`
}

func activitySnapshot(t *testing.T, f statusFixture) (uuid.UUID, activityBody, int64) {
	t.Helper()
	var id uuid.UUID
	var raw []byte
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,payload,desired_revision FROM slack_posts WHERE thread_id=$1 AND role='intermediate' AND continuation_ordinal=0`, f.thread).Scan(&id, &raw, &revision); err != nil {
		t.Fatal(err)
	}
	var body activityBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return id, body, revision
}

func TestOwnedActivityPreservesContentAndShowsPhysicalStop(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.link(t)
	write := f.outputWriter(t)
	child, turn := activityChild(t, f, true)
	attachment, err := session.AcquireRuntimeAttachment(t.Context(), f.Pool, activityHost(child), activityExecution(child))
	if err != nil {
		t.Fatal(err)
	}
	if err = session.RuntimeAsk(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, activityHost(child), activityExecution(child), turn.TurnID, uuid.NewV7(), json.RawMessage(`{"prompt":[{"type":"text","text":"Must not become a public ask"}],"type":"text"}`)); !errors.Is(err, session.ErrRootSessionRequired) {
		t.Fatal(err)
	}
	write("Original front explanation.")
	if _, err = session.RuntimeOutput(t.Context(), f.Pool, activityHost(child), activityExecution(child), turn.TurnID, uuid.NewV7(), json.RawMessage(`[{"type":"text","text":"PRIVATE CHILD OUTPUT"}]`)); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	id, before, revision := activitySnapshot(t, f)
	if len(before.Blocks) != 2 || before.Blocks[1].TaskID != turn.TurnID.String() || before.Blocks[1].Status != "in_progress" || !strings.Contains(before.Text, "Original front explanation.") || strings.Contains(before.Text, "PRIVATE") {
		t.Fatal("wrong work message", before)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE session_id=$1`, child.Session).Scan(&count); err != nil || count != 0 {
		t.Fatalf("child transcript published: %d %v", count, err)
	}
	projectAllActivity(t, f)
	_, same, sameRevision := activitySnapshot(t, f)
	if sameRevision != revision || same.Blocks[1].BlockID != before.Blocks[1].BlockID {
		t.Fatal("unchanged activity revised")
	}
	claim := f.postClaim(t, id)
	if err = FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
		t.Fatal(err)
	}
	request := f.nativeStop(t, "stop-child")
	if err = admitNativeStop(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, request); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	afterID, stopping, next := activitySnapshot(t, f)
	if afterID != id || next <= revision || stopping.Blocks[1].Status != "in_progress" || !strings.Contains(stopping.Text, "Stopping") || stopping.Blocks[1].TaskID != before.Blocks[1].TaskID || stopping.Blocks[1].BlockID == before.Blocks[1].BlockID {
		t.Fatal("stop claim overstated cessation", stopping)
	}
	if err = session.ObserveSessionStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, activityHost(child), activityExecution(child), attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, stopped, _ := activitySnapshot(t, f)
	if stopped.Blocks[1].Status != "error" || !strings.Contains(stopped.Text, "Stopped.") {
		t.Fatal("confirmed stop did not converge", stopped)
	}
}

func TestOwnedActivityBoundsRowsWithoutOpeningPosts(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	write := f.outputWriter(t)
	for range activityCardsPerPost + 2 {
		activityChild(t, f, false)
	}
	projectAllActivity(t, f)
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND role<>'controls'`, f.thread).Scan(&count); err != nil || count != 0 {
		t.Fatalf("child alone opened post: %d %v", count, err)
	}
	write("Original explanation.")
	projectAllActivity(t, f)
	_, body, _ := activitySnapshot(t, f)
	cards := 0
	for _, block := range body.Blocks {
		if block.Type == "task_card" {
			cards++
		}
	}
	if cards != activityCardsPerPost || !strings.Contains(body.Text, "2 more child tasks in Console") || strings.Contains(body.Text, "PRIVATE") {
		t.Fatal("unbounded or private child detail", body)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_posts WHERE thread_id=$1 AND role<>'controls'`, f.thread).Scan(&count); err != nil || count != 1 {
		t.Fatalf("extra post: %d %v", count, err)
	}
}

func TestOwnedActivityKeepsFirstAppearanceAcrossQuestion(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	write := f.outputWriter(t)
	_, first := activityChild(t, f, false)
	write("Before the question.")
	projectAllActivity(t, f)
	var owner uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT post_id FROM slack_work_items WHERE turn_id=$1`, first.TurnID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	ask := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, activityHost(f), activityExecution(f), turn, ask, json.RawMessage(`{"type":"text","prompt":[{"type":"text","text":"Continue?"}]}`)); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	if _, err := session.AnswerAsk(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, AnswerID: uuid.NewV7().String(), Answer: json.RawMessage(`"yes"`)}); err != nil {
		t.Fatal(err)
	}
	_, second := activityChild(t, f, false)
	write("After the answer.")
	projectAllActivity(t, f)
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT a.post_id=$3 AND b.post_id<>a.post_id AND p.closed_at IS NOT NULL FROM slack_work_items a JOIN slack_work_items b ON b.turn_id=$2 JOIN slack_posts p ON p.id=a.post_id WHERE a.turn_id=$1`, first.TurnID, second.TurnID, owner).Scan(&correct); err != nil || !correct {
		t.Fatalf("child moved across question: %v %v", correct, err)
	}
}

func TestOwnedActivityDoesNotCopyCardsIntoNarrativeContinuations(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	write := f.outputWriter(t)
	_, child := activityChild(t, f, false)
	write("First explanation.")
	projectAllActivity(t, f)
	write(strings.Repeat("x", 5000))
	projectAllActivity(t, f)
	rows, err := f.Pool.Query(t.Context(), `SELECT p.id,p.payload,(SELECT post_id FROM slack_work_items WHERE turn_id=$2) FROM slack_posts p WHERE p.thread_id=$1 ORDER BY seq`, f.thread, child.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, owner uuid.UUID
		var raw []byte
		if err = rows.Scan(&id, &raw, &owner); err != nil {
			t.Fatal(err)
		}
		var body activityBody
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, block := range body.Blocks {
			if block.Type == "task_card" {
				count++
				if id != owner {
					t.Fatal("card copied to continuation")
				}
			}
		}
	}
	if err = rows.Err(); err != nil || count != 1 {
		t.Fatalf("card occurrences=%d: %v", count, err)
	}
}

func TestOwnedActivityTurnInterruptAcknowledgementStopsOnlyThatCard(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	write := f.outputWriter(t)
	child, turn := activityChild(t, f, true)
	write("Original explanation.")
	projectAllActivity(t, f)
	_, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: child.Session, TurnID: turn.TurnID, Kind: "turn_interrupt", IdempotencyKey: "stop-only-turn"})
	if err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, pending, _ := activitySnapshot(t, f)
	if !strings.Contains(pending.Text, "Stopping") {
		t.Fatal("stop prematurely confirmed", pending)
	}
	execution := activityExecution(child)
	if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, child.Session).Scan(&execution.AuthorityGeneration); err != nil {
		t.Fatal(err)
	}
	delivery, err := session.PrepareSessionDelivery(t.Context(), f.Pool, activityHost(child), execution, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AcknowledgeSessionDelivery(t.Context(), f.Pool, activityHost(child), execution, 1, delivery.Sequence, delivery.Generation, delivery.Kind, "", delivery.StopSequence); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, stopped, _ := activitySnapshot(t, f)
	if stopped.Blocks[1].Status != "error" || !strings.Contains(stopped.Text, "Stopped.") {
		t.Fatal("turn-only stop never converged", stopped)
	}
	next, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: child.Session, IdempotencyKey: "next-child-turn", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchRuntimeTurn(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: child.Session, TurnID: next.TurnID, Kind: "turn_interrupt", IdempotencyKey: "stop-next-turn"}); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, nextStopping, _ := activitySnapshot(t, f)
	if len(nextStopping.Blocks) != 3 || nextStopping.Blocks[1].Status != "error" || nextStopping.Blocks[2].Status != "in_progress" {
		t.Fatal("later turn reopened old stop", nextStopping)
	}
	// A new unrelated Session hold must not reopen the old Turn's stop observation.
	_, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: child.Session, Kind: "interrupt", IdempotencyKey: "later-hold", Reason: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, later, _ := activitySnapshot(t, f)
	if later.Blocks[1].Status != "error" || !strings.Contains(later.Text, "Stopped.") {
		t.Fatal("later hold reopened old stop", later)
	}
}

func TestOwnedActivityEncodedBoundIncludesEscapedNarrative(t *testing.T) {
	for _, text := range []string{strings.Repeat("<>&", 2000), strings.Repeat("\x00", 6000)} {
		f := newSlackAdmissionFixture(t)
		write := f.outputWriter(t)
		for range activityCardsPerPost {
			activityChild(t, f, false)
		}
		write(text)
		projectAllActivity(t, f)
		var maximum int
		if err := f.Pool.QueryRow(t.Context(), `SELECT max(octet_length(payload)) FROM slack_posts WHERE thread_id=$1`, f.thread).Scan(&maximum); err != nil || maximum > 65536 {
			t.Fatalf("encoded size=%d: %v", maximum, err)
		}
	}
}

func TestOwnedActivityDoesNotConfirmUnceasedCancellationOrLostInterrupt(t *testing.T) {
	for _, kind := range []string{"cancel", "turn_interrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			write := f.outputWriter(t)
			child, turn := activityChild(t, f, true)
			write("Original explanation.")
			projectAllActivity(t, f)
			req := session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: child.Session, Kind: kind, IdempotencyKey: "stop-child"}
			if kind == "turn_interrupt" {
				req.TurnID = turn.TurnID
			}
			if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), req); err != nil {
				t.Fatal(err)
			}
			projectAllActivity(t, f)
			_, stopping, _ := activitySnapshot(t, f)
			if stopping.Blocks[1].Status != "in_progress" || !strings.Contains(stopping.Text, "Stopping") {
				t.Fatal("premature cessation", stopping)
			}
			if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				return session.LoseProcess(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, tx, activityExecution(child), false)
			}); err != nil {
				t.Fatal(err)
			}
			projectAllActivity(t, f)
			_, lost, _ := activitySnapshot(t, f)
			if lost.Blocks[1].Status != "in_progress" || !strings.Contains(lost.Text, "Status unavailable") {
				t.Fatal("lost process shown stopped", lost)
			}
		})
	}
}

func TestOwnedActivityProcessFailureDoesNotImplyCessation(t *testing.T) {
	for _, kind := range []string{"loss", "runtime-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			write := f.outputWriter(t)
			child, _ := activityChild(t, f, true)
			write("Original explanation.")
			projectAllActivity(t, f)
			var err error
			if kind == "loss" {
				err = db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
					return session.LoseProcess(t.Context(), auditpolicy.Policy{
						RetentionSeconds: 3600,
					}, tx, activityExecution(child), false)
				})
			} else {
				err = session.RuntimeHold(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, f.Pool, activityHost(child), activityExecution(child), "fixture native failure")
			}
			if err != nil {
				t.Fatal(err)
			}
			projectAllActivity(t, f)
			_, body, _ := activitySnapshot(t, f)
			want := "Status unavailable"
			if kind == "runtime-failure" {
				want = "Stopping"
			}
			if body.Blocks[1].Status != "in_progress" || !strings.Contains(body.Text, want) {
				t.Fatal("failure implied cessation", body)
			}
		})
	}
}

func TestOwnedActivitySessionCallerUsesAdmissionOrdering(t *testing.T) {
	for _, order := range []string{"output-child-question", "output-question-child", "question-output-child", "child-output"} {
		t.Run(order, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			write := f.outputWriter(t)
			var child session.Admission
			question := func() {
				var turn uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
					t.Fatal(err)
				}
				ask := uuid.NewV7()
				if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{
					RetentionSeconds: 3600,
				}, f.Pool, activityHost(f), activityExecution(f), turn, ask, json.RawMessage(`{"type":"text","prompt":[{"type":"text","text":"Continue?"}]}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := session.AnswerAsk(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, AnswerID: uuid.NewV7().String(), Answer: json.RawMessage(`"yes"`)}); err != nil {
					t.Fatal(err)
				}
			}
			for step := range strings.SplitSeq(order, "-") {
				switch step {
				case "output":
					write("Front explanation.")
				case "question":
					question()
				case "child":
					_, child = activityChildOrigin(t, f, false, false)
				}
			}
			// Project the child before the front backlog, matching asynchronous delivery.
			var participant uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_thread_sources WHERE session_id=$1`, child.SessionID).Scan(&participant); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				if _, err := projectParticipant(t.Context(), f.Pool, participant, testProjectionConfig()); err != nil {
					t.Fatal(err)
				}
			}
			projectAllActivity(t, f)
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_work_items WHERE turn_id=$1`, child.TurnID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if order == "output-child-question" || order == "question-output-child" {
				want = 1
			}
			if count != want {
				t.Fatalf("attachments=%d, want %d", count, want)
			}
			var origin *uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT origin_turn_id FROM turns WHERE id=$1`, child.TurnID).Scan(&origin); err != nil || origin != nil {
				t.Fatalf("invented origin: %v %v", origin, err)
			}
		})
	}
}

func TestOwnedActivitySessionCallerSuccessorsUseTheirOwnAdmission(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	write := f.outputWriter(t)
	write("First explanation.")
	child, first := activityChildOrigin(t, f, false, false)
	projectAllActivity(t, f)
	var firstPost uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT post_id FROM slack_work_items WHERE turn_id=$1`, first.TurnID).Scan(&firstPost); err != nil {
		t.Fatal(err)
	}
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	ask := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.Pool, activityHost(f), activityExecution(f), turn, ask, json.RawMessage(`{"type":"text","prompt":[{"type":"text","text":"Continue?"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.AnswerAsk(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, AskID: ask, AnswerID: uuid.NewV7().String(), Answer: json.RawMessage(`"yes"`)}); err != nil {
		t.Fatal(err)
	}
	write("Second explanation.")
	host := activityHost(f)
	second, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "session", MCPScopeID: f.MCPScope, ID: f.Session, Execution: activityExecution(f), Host: &host}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: child.Session, IdempotencyKey: "successor", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	userTurn, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: child.Session, IdempotencyKey: "user-successor", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	var secondPost uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT post_id FROM slack_work_items WHERE turn_id=$1`, second.TurnID).Scan(&secondPost); err != nil || secondPost == firstPost {
		t.Fatalf("wrong successor attachment %v %v", secondPost, err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_work_items WHERE turn_id=$1`, userTurn.TurnID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("user turn attached: %d %v", count, err)
	}
	// A caller with no active front Turn cannot borrow a previous explanation.
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: turn, Kind: "turn_interrupt", IdempotencyKey: "finish-front"}); err != nil {
		t.Fatal(err)
	}
	execution := activityExecution(f)
	if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, f.Session).Scan(&execution.AuthorityGeneration); err != nil {
		t.Fatal(err)
	}
	later, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.Caller{Kind: "session", MCPScopeID: f.MCPScope, ID: f.Session, Execution: execution, Host: &host}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: child.Session, IdempotencyKey: "inactive-front", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_work_items WHERE turn_id=$1`, later.TurnID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("inactive front attached: %d %v", count, err)
	}
}

func TestDirectActivityRechecksEveryRepresentedAgent(t *testing.T) {
	for _, beforeProjection := range []bool{true, false} {
		t.Run(fmt.Sprintf("before_projection=%v", beforeProjection), func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.link(t)
			f.bindLinkedSource(t)
			f.grantDirect(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $3,environment_id,connection_id,installation_id,org_id,team_id,'D1' FROM slack_channel_bindings WHERE id=$1; UPDATE slack_threads SET slack_channel_binding_id=$3,slack_channel_id='D1',recipient_team_id='team',recipient_user_id='human' WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.channel, f.thread, uuid.NewV7())
			write := f.outputWriter(t)
			child, turn := activityChild(t, f, false)
			otherAgent := uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'private-child-agent');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 UPDATE sessions SET agent_id=$2 WHERE id=$4`, pgx.QueryExecModeSimpleProtocol, f.Environment, otherAgent, f.Agent, child.Session)
			revoke := func() {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE role_grants SET all_agents=false WHERE environment_id=$1;
 INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1 ON CONFLICT DO NOTHING`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Agent)
			}
			write("Visible parent explanation.")
			if beforeProjection {
				revoke()
			}
			projectAllActivity(t, f)
			post, body, _ := activitySnapshot(t, f)
			if beforeProjection {
				if strings.Contains(body.Text, "private-child-agent") {
					t.Fatal("unauthorized Agent metadata projected", body)
				}
				for _, block := range body.Blocks {
					if block.TaskID == turn.TurnID.String() {
						t.Fatal("unauthorized child projected")
					}
				}
			} else {
				if !strings.Contains(body.Text, "private-child-agent") {
					t.Fatal("missing authorized baseline", body)
				}
				revoke()
				claim, err := ClaimPost(t.Context(), f.Pool, post)
				if err != nil || claim != nil {
					t.Fatalf("revoked child metadata claimed: %+v %v", claim, err)
				}
				var suppressed bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT status='suppressed' AND error='publication_authority_unavailable' FROM slack_posts WHERE id=$1`, post).Scan(&suppressed); err != nil || !suppressed {
					t.Fatal("queued composite not suppressed", suppressed, err)
				}
			}
		})
	}
}
