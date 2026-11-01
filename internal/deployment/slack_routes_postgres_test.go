package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

type triggerPreflightFunc func(context.Context, slackroute.StartContext) (json.RawMessage, error)

func (f triggerPreflightFunc) PrepareStart(ctx context.Context, v slackroute.StartContext) (json.RawMessage, error) {
	return f(ctx, v)
}

func TestRegisterAndPromotePinCronSlackConfiguration(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "pin", true: "changed-during-preflight"}[race], func(t *testing.T) {
			f := newDeploymentFinalizePostgresFixture(t)
			route := json.RawMessage(`{"channelId":"C1"}`)
			f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{"tick": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`), Slack: route}}
			// Registration validates syntax without resolving the live Slack connection.
			registered, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
			if err != nil {
				t.Fatal(err)
			}
			// Without a connection the activation waits for Slack, with or without
			// Slack configured, and no live channel check runs.
			unreachable := triggerPreflightFunc(func(context.Context, slackroute.StartContext) (json.RawMessage, error) {
				t.Fatal("waiting trigger performed remote preflight")
				return nil, nil
			})
			for _, preflight := range []slackroute.StartPreflight{nil, unreachable} {
				promotion, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), registered.ID, preflight)
				if err != nil || len(promotion.WaitingTriggers) != 1 || promotion.WaitingTriggers[0] != (WaitingTrigger{Agent: f.request.bundle.bundle.Program.Metadata.Definitions[0].DeclaredID, Trigger: "tick"}) {
					t.Fatal("waiting trigger promotion", promotion, err)
				}
				var waiting bool
				if err = f.pool.QueryRow(t.Context(), `SELECT slack_channel_binding_id IS NULL FROM trigger_activations WHERE deployment_id=$1 AND trigger_key='tick' AND active_until IS NULL`, registered.ID).Scan(&waiting); err != nil || !waiting {
					t.Fatal("waiting activation", waiting, err)
				}
			}
			installation, registration, publication, user := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner');
 INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES('http',$2,$3,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$1);
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($4,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$1);
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) SELECT $5,$6,id,$2,$4,$1,(SELECT org_id FROM slack_app_registrations WHERE id=$2) FROM agents WHERE environment_id=$6 AND key=$7`, pgx.QueryExecModeSimpleProtocol, user, registration, f.request.orgID, installation, publication, f.request.environmentID, f.request.bundle.bundle.Program.Metadata.Definitions[0].DeclaredID)
			calls := 0
			preflight := triggerPreflightFunc(func(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
				calls++
				if value.Route.Target.InstallationID != installation || value.Route.Target.ChannelID != "C1" {
					t.Fatal("wrong scheduled preflight", value)
				}
				// An independent write proves the remote-access stage holds no SQL gate.
				if _, err := f.pool.Exec(ctx, `UPDATE environments SET updated_at=clock_timestamp() WHERE id=$1`, f.request.environmentID); err != nil {
					return nil, err
				}
				if race {
					if _, err := f.pool.Exec(ctx, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, publication); err != nil {
						return nil, err
					}
				}
				return json.RawMessage(`{"text":"Started agent."}`), nil
			})
			promotion, err := Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), registered.ID, preflight)
			if race {
				if !errors.Is(err, slackroute.ErrConversationChanged) {
					t.Fatal(err)
				}
				var clean bool
				if err = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*)=1 AND bool_and(slack_channel_binding_id IS NULL) FROM trigger_activations WHERE active_until IS NULL) AND NOT EXISTS(SELECT 1 FROM slack_channel_bindings) FROM environments WHERE id=$1`, f.request.environmentID).Scan(&clean); err != nil || !clean {
					t.Fatal("failed preflight changed activations", clean, err)
				}
				return
			}
			if err != nil || calls != 1 || len(promotion.WaitingTriggers) != 0 {
				t.Fatal(calls, promotion, err)
			}
			var pin uuid.UUID
			if err = f.pool.QueryRow(t.Context(), `SELECT c.id FROM trigger_activations s JOIN slack_channel_bindings c ON c.id=s.slack_channel_binding_id WHERE s.deployment_id=$1 AND s.trigger_key='tick' AND c.connection_id=$2 AND c.slack_channel_id='C1'`, registered.ID, publication).Scan(&pin); err != nil {
				t.Fatal("promotion lost destination", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, publication)
			if receipt, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request); err != nil || receipt.ID != registered.ID {
				t.Fatal("lost exact registration", receipt, err)
			}
			f.request.idempotencyKey = "new-deployment"
			f.request.bundle.root.Digest = artifacttest.Digest("new-slack-bundle")
			newer, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
			if err != nil {
				t.Fatal("offline registration required live connection", err)
			}
			if promotion, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), newer.ID, preflight); err != nil || len(promotion.WaitingTriggers) != 1 {
				t.Fatal("revoked connection did not leave the trigger waiting", promotion, err)
			}
			if calls != 1 {
				t.Fatal("waiting route performed remote preflight")
			}
		})
	}
}

// A live connection that cannot use the declared channel still fails
// promotion; the operator has no later prompt to fix the channel.
func TestPromoteFailsOnUnusableChannelOfLiveConnection(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{"tick": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`), Slack: json.RawMessage(`{"channelId":"C1"}`)}}
	registered, err := register(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	user, registration, installation := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner');
 INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES('http',$2,$3,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$1);
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($4,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$1);
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) SELECT $5,$6,id,$2,$4,$1,$3 FROM agents WHERE environment_id=$6 AND key=$7`, pgx.QueryExecModeSimpleProtocol, user, registration, f.request.orgID, installation, uuid.NewV7(), f.request.environmentID, f.request.bundle.bundle.Program.Metadata.Definitions[0].DeclaredID)
	preflight := triggerPreflightFunc(func(context.Context, slackroute.StartContext) (json.RawMessage, error) {
		return nil, slackroute.ErrChannelUnavailable
	})
	if _, err = Promote(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, f.principal(), f.scope(), registered.ID, preflight); !errors.Is(err, slackroute.ErrChannelUnavailable) {
		t.Fatal("unusable channel promoted", err)
	}
	var unchanged bool
	if err = f.pool.QueryRow(t.Context(), `SELECT current_deployment_id IS NULL AND NOT EXISTS(SELECT 1 FROM trigger_activations) FROM environments WHERE id=$1`, f.request.environmentID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("failed promotion changed state", unchanged, err)
	}
}
