package slack

import (
	"bytes"
	"encoding/json"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func testProjectionConfig() ProjectionConfig {
	return ProjectionConfig{PublicURL: &url.URL{Scheme: "https", Host: "console.example.test"}, ControlKey: bytes.Repeat([]byte{4}, 32)}
}

func TestSlackAdmissionKeepsReceiptsWithoutPublicAcceptancePosts(t *testing.T) {
	for _, operation := range []string{"message", "new_root", "stop", "answer"} {
		t.Run(operation, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.link(t)
			var id uuid.UUID
			switch operation {
			case "message":
				id = f.reply(t, "message", false)
			case "new_root":
				id = f.newMessage(t, "root", "")
			case "stop":
				id = f.controlReceipt(t, f.control("stop"), nil)
			case "answer":
				c := f.control("answer")
				c.Target.Turn, c.Target.Ask = f.pendingQuestion(t)
				id = f.controlReceipt(t, c, json.RawMessage(`"yes"`))
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE slack_posts ADD CONSTRAINT no_receipt_posts CHECK(role<>'request_feedback')`)
			for range 2 {
				if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
					t.Fatal(err)
				}
			}
			var accepted bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='accepted' AND NOT EXISTS(SELECT 1 FROM slack_posts WHERE request_id=$1) FROM slack_requests WHERE id=$1`, id).Scan(&accepted); err != nil || !accepted {
				t.Fatal("missing receipt or public acceptance post", accepted, err)
			}
		})
	}
}

func TestSlackQuietAdmissionStillProjectsNativeStatusAndAuthoredContent(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	write := f.outputWriter(t)
	id := f.reply(t, "message", false)
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimStatus(t.Context(), f.Pool, f.thread)
	if err != nil || claim == nil || !bytes.Contains(claim.Payload, []byte("processing")) {
		t.Fatal(claim, err)
	}
	write("Here is the deliberate progress update.")
	f.projectAll(t)
	var raw []byte
	var postID uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT id,payload FROM slack_posts WHERE thread_source_id=$1 AND role='intermediate'`, f.participant).Scan(&postID, &raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("Here is the deliberate progress update.")) || bytes.Contains(raw, []byte(`"type": "actions"`)) || bytes.Contains(raw, []byte("helmr.stop")) || bytes.Contains(raw, []byte("View in Console")) {
		t.Fatal(string(raw))
	}
	if err = FinishStatus(t.Context(), f.Pool, *claim, DeliveryResult{Disposition: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	post, err := ClaimPost(t.Context(), f.Pool, postID)
	if err != nil || post == nil {
		t.Fatal("first authored content could not publish", post, err)
	}
}
