package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestCrossAppStartVerifiesBothSendersAndPinsOpening(t *testing.T) {
	for _, kind := range []string{"valid", "source-stopped", "target-stopped", "unknown-opening", "source-revoked-after-issued", "source-disconnected-before-root-recognition", "pending-source", "unpublished-target", "workspace-mismatch", "target-denied", "source-denied", "source-revoked", "permalink-unavailable", "too-large"} {
		t.Run(kind, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			f.outputWriter(t)
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE session_id=$1 AND status='running'`, f.Session).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			target, registration, installation, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,name) VALUES($1,$2,'target');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers) SELECT environment_id,$2,deployment_id,'target',computer_definition_key,setup,triggers FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) SELECT $4,org_id,'target-app','target-client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$7 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) SELECT $5,$4,org_id,'target-app','team','target-bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$7 FROM environments WHERE id=$1;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($6,$1,$2,$4,$5,$7)`, pgx.QueryExecModeSimpleProtocol, f.Environment, target, f.Agent, registration, installation, publication, f.User)
			switch kind {
			case "pending-source":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
			case "unpublished-target":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, publication)
			case "workspace-mismatch":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET team_id='other-team' WHERE id=$1`, installation)
			}
			calls := 0
			client := NewWebClient(credentialsFunc(func(_ context.Context, id uuid.UUID, revision int64) (string, error) {
				if revision != 1 {
					t.Fatal("unexpected revision", revision)
				}
				if id == f.installation {
					return "source-token", nil
				}
				if id == installation {
					return "target-token", nil
				}
				t.Fatal("unexpected sender", id)
				return "", nil
			}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Query().Get("channel") != "C1" {
					t.Fatal("unexpected request", r.Method, r.URL)
				}
				token := r.Header.Get("Authorization")
				var body any
				switch r.URL.Path {
				case "/api/conversations.info":
					member := !(kind == "target-denied" && token == "Bearer target-token" || kind == "source-denied" && token == "Bearer source-token")
					body = map[string]any{"ok": true, "channel": map[string]any{"id": "C1", "name": "work", "context_team_id": "team", "is_channel": true, "is_member": member}}
				case "/api/chat.getPermalink":
					if token != "Bearer source-token" || r.URL.Query().Get("message_ts") != "123.456" {
						t.Fatal("permalink used target identity", r.URL, token)
					}
					if kind == "source-revoked" {
						dbtest.MustExec(t, r.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
					}
					body = map[string]any{"ok": kind != "permalink-unavailable", "permalink": "https://workspace.slack.com/archives/C1/p123456"}
				default:
					t.Fatal("unexpected method", r.URL)
				}
				raw, _ := json.Marshal(body)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			}))
			input := json.RawMessage(`[{"type":"text","text":"Keep <@everyone> literal"},{"type":"text","text":"\nsecond part"}]`)
			if kind == "too-large" {
				input, _ = json.Marshal([]map[string]string{{"type": "text", "text": strings.Repeat("x", contentCharacters+1)}})
			}
			caller := agent.Caller{Kind: "session", ID: f.Session, TurnID: turn, Execution: agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}, Host: &workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
			result, err := agent.Start(t.Context(), f.Pool, nil, caller, agent.StartRequest{EnvironmentID: f.Environment, Agent: "target", ComputerID: f.Computer, RetryKey: "cross-app", Input: input, SlackPreflight: client})
			accepted := kind == "valid" || kind == "source-stopped" || kind == "target-stopped" || kind == "unknown-opening" || kind == "source-revoked-after-issued" || kind == "source-disconnected-before-root-recognition"
			if !accepted {
				want := map[string]error{"pending-source": agent.ErrSourceConversationPending, "unpublished-target": agent.ErrTargetNotPublished, "workspace-mismatch": agent.ErrTargetWorkspaceMismatch, "target-denied": agent.ErrSlackChannelUnavailable, "source-denied": agent.ErrSlackChannelUnavailable, "source-revoked": agent.ErrConversationChanged, "permalink-unavailable": agent.ErrSourceConversationUnavailable, "too-large": agent.ErrStartMessageTooLarge}[kind]
				if !errors.Is(err, want) {
					t.Fatal("wrong rejection", err, want)
				}
				var clean bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions)=1 AND NOT EXISTS(SELECT 1 FROM slack_posts)`).Scan(&clean); err != nil || !clean {
					t.Fatal("rejected preflight admitted work", clean, err)
				}
				if kind == "pending-source" || kind == "unpublished-target" || kind == "workspace-mismatch" {
					if calls != 0 {
						t.Fatal("local rejection made remote calls", calls)
					}
				}
				return
			}
			if err != nil || calls != 3 {
				t.Fatal(calls, err)
			}
			if kind == "source-stopped" || kind == "target-stopped" {
				stopped := f.Session
				if kind == "target-stopped" {
					stopped = result.SessionID
				}
				if _, err = agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: stopped, Kind: "cancel", RetryKey: "stop-after-start"}); err != nil {
					t.Fatal(err)
				}
				if kind == "source-stopped" {
					var independent bool
					if err = f.Pool.QueryRow(t.Context(), `SELECT status='open' FROM sessions WHERE id=$1`, result.SessionID).Scan(&independent); err != nil || !independent {
						t.Fatal("source cancellation reached independent target", independent, err)
					}
				}
			}
			var opening uuid.UUID
			var payload []byte
			peer := f
			peer.Session = result.SessionID
			if err = f.Pool.QueryRow(t.Context(), `SELECT p.id,p.payload,t.id,src.id FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_thread_sources src ON src.thread_id=t.id AND src.session_id=t.front_session_id WHERE p.session_id=$1 AND p.role='opening' AND p.source_thread_id=$2`, result.SessionID, f.thread).Scan(&opening, &payload, &peer.thread, &peer.participant); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Blocks []struct {
					Elements []struct {
						Elements []struct {
							Type string `json:"type"`
							Text string `json:"text"`
							User string `json:"user_id"`
						} `json:"elements"`
					} `json:"elements"`
				} `json:"blocks"`
			}
			if err = json.Unmarshal(payload, &body); err != nil {
				t.Fatal(err)
			}
			elements := body.Blocks[0].Elements[0].Elements
			if len(elements) != 4 || elements[1].Type != "user" || elements[1].User != "target-bot" || elements[2].Type != "text" || elements[2].Text != "\nKeep <@everyone> literal\nsecond part\n" {
				t.Fatalf("opening changed content or mention authority: %s", payload)
			}
			claim := peer.postClaim(t, opening)
			if claim.InstallationID != f.installation {
				t.Fatal("opening used target sender")
			}
			later := peer.post(t, 2, "target-response", "lifecycle", nil)
			peer.noPostClaim(t, later)
			if kind == "unknown-opening" || kind == "source-revoked-after-issued" || kind == "source-disconnected-before-root-recognition" {
				if err = FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}); err != nil {
					t.Fatal(err)
				}
				if kind == "source-revoked-after-issued" {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, f.publication)
				}
				peer.due(t)
				peer.noPostClaim(t, opening)
				peer.noPostClaim(t, later)
				if ok, err := confirmMessage(t.Context(), f.Pool, installation, observedClaim(t, claim)); err != nil || ok {
					t.Fatal("target identity confirmed source opening", ok, err)
				}
				if kind == "source-disconnected-before-root-recognition" {
					store, err := NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32))
					if err != nil {
						t.Fatal(err)
					}
					var org uuid.UUID
					if err = f.Pool.QueryRow(t.Context(), `SELECT organization_id FROM slack_installations WHERE id=$1`, f.installation).Scan(&org); err != nil {
						t.Fatal(err)
					}
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE org_id=$1 AND user_id=$2`, org, f.User)
					if err = store.DisconnectPublication(t.Context(), org, f.User, f.Environment, f.publication); err != nil {
						t.Fatal(err)
					}
					f.link(t)
					targetFront := peer
					targetFront.installation = installation
					gesture := targetFront.newMessage(t, "resolve-disconnected-source-opening", "123.789")
					message := observedClaim(t, claim)
					client := NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path != "/api/conversations.history" {
							return f.admissionClient(t).http.Do(r)
						}
						raw, _ := json.Marshal(map[string]any{"ok": true, "messages": []observedMessage{message}})
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
					}))
					if err = admitMessage(t.Context(), f.Pool, nil, client, gesture); err != nil {
						t.Fatal(err)
					}
					var preserved bool
					if err = f.Pool.QueryRow(t.Context(), `SELECT t.thread_ts='123.789' AND p.status='suppressed' AND p.suppressed_revision=p.desired_revision AND r.status='accepted' AND r.session_id=t.front_session_id FROM slack_threads t JOIN slack_posts p ON p.thread_id=t.id AND p.role='opening' JOIN slack_requests r ON r.id=$2 WHERE t.id=$1`, peer.thread, gesture).Scan(&preserved); err != nil || !preserved {
						t.Fatal("positive recognition revived source or lost target front", preserved, err)
					}
					// Newly authored target content must project after source retirement.
					dbtest.MustExec(t, t.Context(), f.Pool, `WITH advanced AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq) INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) SELECT $1,$2,seq,$3,'turn.output',convert_to('[{"type":"text","text":"target output after source disconnect"}]','UTF8') FROM advanced`, f.Environment, result.SessionID, result.TurnID)
					peer.projectAll(t)
					if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE thread_id=$1 AND role='intermediate' AND status='pending')`, peer.thread).Scan(&preserved); err != nil || !preserved {
						t.Fatal("confirmed target lost new projection", preserved, err)
					}
				} else if ok, err := confirmMessage(t.Context(), f.Pool, f.installation, observedClaim(t, claim)); err != nil || !ok {
					t.Fatal("late source proof did not bind target root", ok, err)
				}
			} else {
				if err = FinishPost(t.Context(), f.Pool, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.999"}); err != nil {
					t.Fatal(err)
				}
			}
			reply := peer.postClaim(t, later)
			if allowed, err := postClaimAuthorized(t.Context(), f.Pool, reply); err != nil || !allowed {
				t.Fatal("target follow-up lost publication authority", allowed, err)
			}
			if reply.InstallationID != installation {
				t.Fatal("target response used source sender")
			}
		})
	}
}
