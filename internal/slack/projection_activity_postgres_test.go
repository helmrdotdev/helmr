package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func activityExecution(f statusFixture) agent.Execution {
	return agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
}
func activityHost(f statusFixture) workergroup.HostPrincipal {
	return workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
}
func activityChild(t *testing.T, f statusFixture, running bool) (statusFixture, agent.Admission) {
	t.Helper()
	var parentTurn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&parentTurn); err != nil {
		t.Fatal(err)
	}
	host := activityHost(f)
	caller := agent.Caller{Kind: "session", ID: f.Session, TurnID: parentTurn, Execution: activityExecution(f), Host: &host}
	receipt, err := agent.Spawn(t.Context(), f.Pool, nil, caller, agent.StartRequest{EnvironmentID: f.Environment, Agent: "agent", ComputerID: f.Computer, RetryKey: uuid.NewV7().String(), Input: json.RawMessage(`[{"type":"text","text":"PRIVATE CHILD ASSIGNMENT"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	child := f
	child.Session = receipt.SessionID
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_thread_sources WHERE session_id=$1`, child.Session).Scan(&child.participant); err != nil {
		t.Fatal(err)
	}
	if running {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$3,1,'ready')`, f.Environment, child.Session, f.Computer)
		if _, err = agent.Dispatch(t.Context(), f.Pool, activityExecution(child)); err != nil {
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,payload,desired_revision FROM slack_posts WHERE thread_id=$1 AND role='activity' AND continuation_ordinal=0`, f.thread).Scan(&id, &raw, &revision); err != nil {
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
	f.outputWriter(t)
	child, turn := activityChild(t, f, true)
	attachment, err := agent.AcquireRuntimeAttachment(t.Context(), f.Pool, activityHost(child), activityExecution(child))
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RuntimeAsk(t.Context(), f.Pool, activityHost(child), activityExecution(child), turn.TurnID, uuid.NewV7(), json.RawMessage(`{"prompt":[{"type":"text","text":"Must not become a public ask"}],"answer":{"type":"text"}}`)); !errors.Is(err, agent.ErrRootSessionRequired) {
		t.Fatal(err)
	}
	authored := strings.Repeat("child progress 😀 ", 400)
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": authored}, {"type": "json", "value": map[string]string{"public": "explicit output"}}})
	if _, err = agent.RuntimeOutput(t.Context(), f.Pool, activityHost(child), activityExecution(child), turn.TurnID, uuid.NewV7(), content); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	id, before, revision := activitySnapshot(t, f)
	if len(before.Blocks) != 3 || before.Blocks[1].TaskID != turn.TurnID.String() || before.Blocks[1].Status != "in_progress" || !strings.Contains(before.Text, "Working") || strings.Contains(before.Text, "PRIVATE") {
		t.Fatal("wrong activity", before)
	}
	var stop controlEnvelope
	for _, element := range before.Blocks[2].Elements {
		if element.Action == "helmr.stop" {
			stop, err = decodeControl(testProjectionConfig().ControlKey, element.Value, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if stop.Target.Session != f.Session || stop.Target.Participant != f.participant {
		t.Fatal("activity stop targeted child", stop.Target)
	}
	rows, err := f.Pool.Query(t.Context(), `SELECT payload,presentation_path FROM slack_posts WHERE session_id=$1 AND role='intermediate' ORDER BY seq`, child.Session)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	count := 0
	for rows.Next() {
		var raw []byte
		var path string
		if err = rows.Scan(&raw, &path); err != nil {
			t.Fatal(err)
		}
		var body MessageBody
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, block := range body.Blocks {
			if block.Type == "rich_text" {
				for _, section := range block.Elements {
					for _, part := range section.Elements {
						if part.Type == "text" {
							text.WriteString(part.Text)
						}
					}
				}
			}
		}
		if path != "post" || !bytes.Contains(raw, []byte("Delegated work")) || bytes.Contains(raw, []byte("helmr.open_answer")) {
			t.Fatal("child content impersonated front", path, string(raw))
		}
		count++
	}
	rows.Close()
	if count < 2 || !strings.Contains(text.String(), "explicit output") || !strings.Contains(text.String(), authored) {
		t.Fatal("child content truncated", count)
	}
	projectAllActivity(t, f)
	_, same, sameRevision := activitySnapshot(t, f)
	if sameRevision != revision || same.Blocks[1].BlockID != before.Blocks[1].BlockID {
		t.Fatal("unchanged activity revised")
	}
	claim := f.postClaim(t, id)
	if err = FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
		t.Fatal(err)
	}
	request := f.controlReceipt(t, stop, nil)
	if err = admitControl(t.Context(), f.Pool, request); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	afterID, stopping, next := activitySnapshot(t, f)
	if afterID != id || next <= revision || stopping.Blocks[1].Status != "in_progress" || !strings.Contains(stopping.Text, "Stopping") || stopping.Blocks[1].TaskID != before.Blocks[1].TaskID || stopping.Blocks[1].BlockID == before.Blocks[1].BlockID {
		t.Fatal("stop claim overstated cessation", stopping)
	}
	if err = agent.ObserveSessionStopped(t.Context(), f.Pool, activityHost(child), activityExecution(child), attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	projectAllActivity(t, f)
	_, stopped, _ := activitySnapshot(t, f)
	if stopped.Blocks[1].Status != "error" || !strings.Contains(stopped.Text, "Stopped.") {
		t.Fatal("confirmed stop did not converge", stopped)
	}
}

func TestOwnedActivityPagesEveryAttemptWithoutPublicInputs(t *testing.T) {
	f := newSlackAdmissionFixture(t)
	f.outputWriter(t)
	ids := map[string]bool{}
	for range activityCardsPerPost + 1 {
		_, turn := activityChild(t, f, false)
		ids[turn.TurnID.String()] = false
	}
	projectAllActivity(t, f)
	rows, err := f.Pool.Query(t.Context(), `SELECT payload FROM slack_posts WHERE thread_id=$1 AND role='activity' ORDER BY continuation_ordinal`, f.thread)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	pages := 0
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var body activityBody
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, block := range body.Blocks {
			if block.Type == "task_card" {
				seen, ok := ids[block.TaskID]
				if !ok || seen {
					t.Fatal("missing or duplicate task", block.TaskID)
				}
				ids[block.TaskID] = true
			}
		}
		if strings.Contains(body.Text, "PRIVATE") {
			t.Fatal("assignment published")
		}
		pages++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Fatal(pages)
	}
	for id, seen := range ids {
		if !seen {
			t.Fatal("attempt omitted", id)
		}
	}
}
