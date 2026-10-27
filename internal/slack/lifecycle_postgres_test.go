package slack

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackLifecycleCommandsRequireExactHumanSyntax(t *testing.T) {
	for _, command := range []string{"sleep", "wake", "close", "help"} {
		for _, text := range []string{"!" + command, "<@bot> !" + command, " \n!" + command + "\n"} {
			if got := threadInputCommand(text, "bot"); got != command {
				t.Fatalf("%q: %q", text, got)
			}
		}
		for _, text := range []string{"`!" + command + "`", "> !" + command, "```\n!" + command + "\n```", "!" + command + " please", "<@other> !" + command} {
			if got := threadInputCommand(text, "bot"); got != "" {
				t.Fatalf("prose executed %q: %q", text, got)
			}
		}
	}
}

func TestSlackLifecycleCommandsConfirmWithoutCreatingWork(t *testing.T) {
	for _, command := range []string{"sleep", "close", "help"} {
		t.Run(command, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			id := f.threadMessage(t, "lifecycle", "!"+command, time.Now())
			f.admitThreadMessage(t, id)
			f.receiptStatus(t, id, "accepted")
			f.assertThreadInput(t, false, 0)
			var controls int
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_controls)+(SELECT count(*) FROM session_controls)`).Scan(&controls); err != nil || controls != 0 {
				t.Fatalf("command mutated execution: %d %v", controls, err)
			}
			feedback, found, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
			if err != nil || !found || feedback == nil {
				t.Fatalf("confirmation missing: %v %v", found, err)
			}
			if !strings.Contains(string(feedback.payload), "blocks") {
				t.Fatalf("no controls: %s", feedback.payload)
			}
		})
	}
}

func TestSlackCloseConfirmationRejectsChangedScopeAndWrongActor(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.threadMessage(t, "close", "!close", time.Now())
	f.admitThreadMessage(t, id)
	feedback, _, err := takeRejectedFeedback(t.Context(), f.Pool, testProjectionConfig())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Blocks []struct {
			Elements []struct {
				Value string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err = json.Unmarshal(feedback.payload, &body); err != nil {
		t.Fatal(err)
	}
	var claims controlEnvelope
	for _, b := range body.Blocks {
		for _, e := range b.Elements {
			if e.Value != "" {
				claims, err = decodeControl(testProjectionConfig().ControlKey, e.Value, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if claims.Action != "confirm_close" || claims.Actor != "human" || claims.Scope == "" {
		t.Fatalf("confirmation unbound: %+v", claims)
	}
	wrongActorRaw, _ := json.Marshal(controlGesture{Control: claims})
	wrongActor, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "other-confirm", Actor: "other-human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Payload: wrongActorRaw})
	if err != nil {
		t.Fatal(err)
	}
	if err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, wrongActor.ID); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus(t, wrongActor.ID, "rejected")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.Session)
	raw, _ := json.Marshal(controlGesture{Control: claims})
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "confirm:" + uuid.NewV7().String(), Actor: "human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, receipt.ID); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus(t, receipt.ID, "rejected")
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, f.Session).Scan(&status); err != nil || status != "open" {
		t.Fatalf("stale confirmation closed Session: %s %v", status, err)
	}
}

func TestSlackLifecycleCardIsRetainedAndRefreshDoesNotCreateWork(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	for range 2 {
		if _, err := projectParticipant(t.Context(), f.Pool, f.participant, testProjectionConfig()); err != nil {
			t.Fatal(err)
		}
	}
	var posts, turns int
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),(SELECT count(*) FROM turns WHERE session_id=$1) FROM slack_posts WHERE thread_id=$2 AND post_key='execution-controls'`, f.Session, f.thread).Scan(&posts, &turns); err != nil || posts != 1 || turns != 0 {
		t.Fatalf("card duplicated or admitted work: %d %d %v", posts, turns, err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_id=$1 AND post_key='execution-controls'`, f.thread).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Automatic close") {
		t.Fatalf("missing effective policy: %s", raw)
	}
}

func TestSlackLifecycleDiscoveryTracksStateWithoutPeriodicCardWrites(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	drain := func() {
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
	drain()
	var before, after int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_revision FROM slack_posts WHERE thread_id=$1 AND post_key='execution-controls'`, f.thread).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		drain()
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_revision FROM slack_posts WHERE thread_id=$1 AND post_key='execution-controls'`, f.thread).Scan(&after); err != nil || after != before {
		t.Fatalf("idle card revised: %d -> %d %v", before, after, err)
	}
	// Promotion must not change the existing front Session's lifecycle policy.
	promoted := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('e',64));
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy) SELECT environment_id,$3,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,suspension,runtime_mcp_policy) SELECT environment_id,agent_id,$3,template_id,has_init,'after_turn',runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE environments SET current_deployment_id=$3 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, promoted)
	drain()
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_revision FROM slack_posts WHERE thread_id=$1 AND post_key='execution-controls'`, f.thread).Scan(&after); err != nil || after != before {
		t.Fatalf("promotion changed pinned lifecycle card: %d -> %d %v", before, after, err)
	}
	// No Session event is emitted for this physical transition.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer)
	drain()
	var payload []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_revision,payload FROM slack_posts WHERE thread_id=$1 AND post_key='execution-controls'`, f.thread).Scan(&after, &payload); err != nil || after != before+1 || !strings.Contains(string(payload), "Computer not yet running") {
		t.Fatalf("physical state not projected: %d -> %d %s %v", before, after, payload, err)
	}
}

func TestSlackLifecycleRendersSampledState(t *testing.T) {
	f := newStatusFixture(t)
	target := controlTarget{Installation: f.installation, Thread: f.thread, Participant: f.participant, Environment: f.Environment, Session: f.Session}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		snapshot, _, err := readLifecycleState(t.Context(), tx, target)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(t.Context(), `UPDATE agent_definitions d SET suspension='after_turn' FROM sessions s WHERE s.id=$1 AND (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)`, f.Session); err != nil {
			return err
		}
		text, _, err := lifecyclePresentationFromState(t.Context(), tx, testProjectionConfig(), target, "help", "", false, snapshot)
		if err == nil && !strings.Contains(text, "Automatic hibernation: off") {
			t.Fatalf("renderer mixed live state into snapshot: %s", text)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSlackUnboundLifecycleCommandsHaveExplicitRejection(t *testing.T) {
	for _, command := range []string{"sleep", "wake", "close", "help"} {
		t.Run(command, func(t *testing.T) {
			f := newStatusFixture(t)
			id := f.threadMessage(t, "unbound-"+command, "!"+command, time.Now())
			if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				return admitLifecycleMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, tx, id, &messageAdmission{}, command)
			}); err != nil {
				t.Fatal(err)
			}
			var code string
			if err := f.Pool.QueryRow(t.Context(), `SELECT error FROM slack_requests WHERE id=$1`, id).Scan(&code); err != nil || code != "command_unbound" {
				t.Fatalf("wrong unbound feedback: %s %v", code, err)
			}
		})
	}
}
