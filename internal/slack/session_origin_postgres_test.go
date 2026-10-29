package slack

import (
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSessionTypedOriginsAdmitOnlySupportedForms(t *testing.T) {
	f := newStatusFixture(t)
	ctx := t.Context()
	key, activation := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) SELECT $1,org_id,project_id,id,'origin','origin',convert_to('origin','UTF8') FROM environments WHERE id=$3;
 INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) SELECT environment_id,agent_id,deployment_id,'daily','* * * * *','UTC','[]' FROM sessions WHERE id=$4;
 INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at) SELECT environment_id,$2,agent_id,deployment_id,'daily',clock_timestamp(),clock_timestamp() FROM sessions WHERE id=$4`, pgx.QueryExecModeSimpleProtocol, key, activation, f.Environment, f.Session)
	for mask := range 16 {
		t.Run(fmt.Sprintf("origins_%04b", mask), func(t *testing.T) {
			var userID, keyID, activationID, connectionID *uuid.UUID
			if mask&1 != 0 {
				userID = &f.User
			}
			if mask&2 != 0 {
				keyID = &key
			}
			if mask&4 != 0 {
				activationID = &activation
			}
			if mask&8 != 0 {
				connectionID = &f.publication
			}
			// Personal human, personal key, scheduled, and shared human+connection.
			want := mask == 1 || mask == 2 || mask == 4 || mask == 9
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = tx.Exec(t.Context(), `UPDATE sessions SET origin_user_id=$2,origin_api_key_id=$3,origin_activation_id=$4,origin_slack_connection_id=$5 WHERE id=$1`, f.Session, userID, keyID, activationID, connectionID)
			if want {
				if err != nil {
					t.Fatalf("supported origin rejected: %v", err)
				}
			} else {
				var constraint *pgconn.PgError
				if !errors.As(err, &constraint) || constraint.Code != "23514" {
					t.Fatalf("unsupported origin accepted: %v", err)
				}
			}
		})
	}
}
