package slack

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
	"uuid"
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
			if command == "help" && (!strings.Contains(string(feedback.payload), "Automatic close") || !strings.Contains(string(feedback.payload), "helmr.lifecycle.sleep")) {
				t.Fatalf("help lacks Computer policy or controls: %s", feedback.payload)
			}
			if command == "sleep" && (!strings.Contains(string(feedback.payload), "Hibernating pauses this Session.") || strings.Contains(string(feedback.payload), f.Session.String())) {
				t.Fatalf("hibernate confirmation lacks its count or lists identities: %s", feedback.payload)
			}
		})
	}
}

func TestSlackCloseConfirmationShowsCountAndRequiresItsActor(t *testing.T) {
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
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
			Elements []struct {
				Value string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err = json.Unmarshal(feedback.payload, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(feedback.payload), "Closing affects this Session only.") {
		t.Fatalf("close confirmation lacks its count: %s", feedback.payload)
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
	if claims.Action != "confirm_close" || claims.Actor != "human" {
		t.Fatalf("confirmation unbound: %+v", claims)
	}
	wrongActorRaw, _ := json.Marshal(controlGesture{Control: claims})
	wrongActor, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "other-confirm", Actor: "other-human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Payload: wrongActorRaw})
	if err != nil {
		t.Fatal(err)
	}
	if err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, wrongActor.ID); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus(t, wrongActor.ID, "rejected")
	raw, _ := json.Marshal(controlGesture{Control: claims})
	receipt, err := receiveGesture(t.Context(), f.Pool, inboundGesture{Installation: f.installation, Key: "confirm:" + uuid.NewV7().String(), Actor: "human", OccurredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err = admitControl(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), f.Pool, receipt.ID); err != nil {
		t.Fatal(err)
	}
	f.receiptStatus(t, receipt.ID, "accepted")
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM sessions WHERE id=$1`, f.Session).Scan(&status); err != nil || status == "open" {
		t.Fatalf("confirmed close left the Session open: %s %v", status, err)
	}
}

func TestSlackUnboundLifecycleCommandsHaveExplicitRejection(t *testing.T) {
	for _, command := range []string{"sleep", "wake", "close", "help"} {
		t.Run(command, func(t *testing.T) {
			f := newStatusFixture(t)
			id := f.threadMessage(t, "unbound-"+command, "!"+command, time.Now())
			if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				return admitLifecycleMessage(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), tx, id, &messageAdmission{}, command)
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
