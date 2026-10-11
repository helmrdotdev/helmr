package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

type schedulePreflightFunc func(context.Context, agent.SlackStartContext) (json.RawMessage, error)

func (f schedulePreflightFunc) PrepareStart(ctx context.Context, v agent.SlackStartContext) (json.RawMessage, error) {
	return f(ctx, v)
}

func TestRegisterAndPromotePinCronSlackConfiguration(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "pin", true: "changed-during-preflight"}[race], func(t *testing.T) {
			f := newDeploymentFinalizePostgresFixture(t)
			route := json.RawMessage(`{"channelId":"C1"}`)
			f.request.bundle.bundle.Program.Metadata.Definitions[0].Agent.Triggers = map[string]definition.CronTrigger{"tick": {Cron: "* * * * *", Timezone: "UTC", Input: []byte(`[]`), Slack: route}}
			// Registration validates syntax without resolving the live Slack connection.
			registered, err := register(t.Context(), f.pool, f.request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), registered.ID, nil); !errors.Is(err, agent.ErrTargetNotPublished) {
				t.Fatal("unpublished schedule promoted", err)
			}
			installation, registration, publication, user := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner');
 INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES($2,$3,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$1);
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($4,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$1);
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) SELECT $5,$6,id,$2,$4,$1 FROM agents WHERE environment_id=$6 AND name=$7`, pgx.QueryExecModeSimpleProtocol, user, registration, f.request.orgID, installation, publication, f.request.environmentID, f.request.bundle.bundle.Program.Metadata.Definitions[0].DeclaredID)
			calls := 0
			preflight := schedulePreflightFunc(func(ctx context.Context, value agent.SlackStartContext) (json.RawMessage, error) {
				calls++
				if value.Route.Target.InstallationID != installation || value.Route.Target.ChannelID != "C1" || value.Route.Source != nil {
					t.Fatal("wrong scheduled preflight", value)
				}
				// An independent write proves the remote-access stage holds no SQL gate.
				if _, err := f.pool.Exec(ctx, `UPDATE environments SET updated_at=clock_timestamp() WHERE id=$1`, f.request.environmentID); err != nil {
					return nil, err
				}
				if race {
					if _, err := f.pool.Exec(ctx, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, publication); err != nil {
						return nil, err
					}
				}
				return json.RawMessage(`{"text":"Started agent."}`), nil
			})
			_, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), registered.ID, preflight)
			if race {
				if !errors.Is(err, agent.ErrConversationChanged) {
					t.Fatal(err)
				}
				var clean bool
				if err = f.pool.QueryRow(t.Context(), `SELECT current_deployment_id IS NULL AND NOT EXISTS(SELECT 1 FROM agent_schedules) AND NOT EXISTS(SELECT 1 FROM slack_channels) FROM environments WHERE id=$1`, f.request.environmentID).Scan(&clean); err != nil || !clean {
					t.Fatal("failed preflight retained activation", clean, err)
				}
				return
			}
			if err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
			var pin uuid.UUID
			if err = f.pool.QueryRow(t.Context(), `SELECT c.id FROM agent_schedules s JOIN slack_channels c ON c.id=s.slack_channel_id WHERE s.deployment_id=$1 AND s.trigger_key='tick' AND c.publication_id=$2 AND c.slack_channel_id='C1'`, registered.ID, publication).Scan(&pin); err != nil {
				t.Fatal("promotion lost destination", err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, publication)
			if receipt, err := register(t.Context(), f.pool, f.request); err != nil || receipt.ID != registered.ID {
				t.Fatal("lost exact registration", receipt, err)
			}
			f.request.retryKey = "new-deployment"
			f.request.bundle.root.Digest = artifacttest.Digest("new-slack-bundle")
			newer, err := register(t.Context(), f.pool, f.request)
			if err != nil {
				t.Fatal("offline registration required live connection", err)
			}
			if _, err = Promote(t.Context(), f.pool, f.principal(), f.scope(), newer.ID, preflight); !errors.Is(err, agent.ErrTargetNotPublished) {
				t.Fatal("retired route promoted", err)
			}
			if calls != 1 {
				t.Fatal("retired route performed remote preflight")
			}
		})
	}
}
