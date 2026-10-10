package slack

import (
	"bytes"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestSlackQuestionWithdrawalPreservesIssuedRevisionAndOtherAsks(t *testing.T) {
	for _, phase := range []string{"pending", "sending", "uncertain"} {
		t.Run(phase, func(t *testing.T) {
			f := newStatusFixture(t)
			turn, ask := f.pendingQuestion(t)
			execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
			host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
			peer := uuid.NewV7()
			if err := agent.RuntimeAsk(t.Context(), f.Pool, host, execution, turn, peer, json.RawMessage(`{"prompt":[{"type":"text","text":"Independent question"}],"answer":{"type":"text"}}`)); err != nil {
				t.Fatal(err)
			}
			f.projectAll(t)
			var post uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM slack_posts WHERE ask_id=$1`, ask).Scan(&post); err != nil {
				t.Fatal(err)
			}
			var first PostClaim
			if phase != "pending" {
				first = f.postClaim(t, post)
				if phase == "uncertain" {
					if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Uncertain}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 {
				if err := agent.RuntimeWithdrawAsk(t.Context(), f.Pool, host, execution, turn, ask); err != nil {
					t.Fatal(err)
				}
			}
			f.projectAll(t)
			f.projectAll(t)
			var body, other, frozen []byte
			if err := f.Pool.QueryRow(t.Context(), `SELECT payload,inflight_payload FROM slack_posts WHERE id=$1`, post).Scan(&body, &frozen); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("helmr.open_answer")) || bytes.Contains(body, []byte("helmr_answer_cli")) || bytes.Count(body, []byte("Question withdrawn.")) != 2 {
				t.Fatal("withdrawn card", string(body))
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE ask_id=$1`, peer).Scan(&other); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(other, []byte("helmr.open_answer")) || !bytes.Contains(other, []byte("helmr_answer_cli")) || bytes.Contains(other, []byte("withdrawn")) {
				t.Fatal("another question changed")
			}
			if phase != "pending" {
				if !bytes.Equal(frozen, first.Payload) {
					t.Fatal("withdrawal changed issued mutation")
				}
				f.due(t)
				f.noPostClaim(t, post)
				if phase == "sending" {
					if err := FinishPost(t.Context(), f.Pool, first, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
						t.Fatal(err)
					}
				} else {
					if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, first)); err != nil || !ok {
						t.Fatalf("positive evidence: %v %v", ok, err)
					}
				}
			}
			f.due(t)
			updated := f.postClaim(t, post)
			want := "chat.update"
			if phase == "pending" {
				want = "chat.postMessage"
			}
			if updated.Method != want || bytes.Contains(updated.Payload, []byte("helmr.open_answer")) || bytes.Contains(updated.Payload, []byte("helmr_answer_cli")) || !bytes.Contains(updated.Payload, []byte("Question withdrawn.")) {
				t.Fatal("withdrawal publication", updated.Method, string(updated.Payload))
			}
			if err := FinishPost(t.Context(), f.Pool, updated, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.790"}); err != nil {
				t.Fatal(err)
			}
			f.projectAll(t)
			f.due(t)
			f.noPostClaim(t, post)
		})
	}
}
