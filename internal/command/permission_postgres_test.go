package command

import (
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestQueuedCommandRechecksCallerPermission(t *testing.T) {
	for _, kind := range []string{"human_grant", "removed_member", "api_key"} {
		t.Run(kind, func(t *testing.T) {
			f := commandFixture(t)
			request := f.request("queued-revocation")
			if kind == "api_key" {
				key := uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'command test','test',$5)`, key, f.OrgID, f.ProjectID, f.Environment, []byte(key.String()))
				dbtest.SetAPIKeyRole(t, f.Pool, key, "computers.execute")
				request.Creator = Creator{Kind: "api_key", ID: key}
			}
			c, err := Create(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, request)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "human_grant":
				f.SetPermissions(t, "sessions.read")
			case "removed_member":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET removed_at=now(),is_owner=false WHERE org_id=$1 AND user_id=$2`, f.OrgID, f.User)
			case "api_key":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=now() WHERE id=$1`, request.Creator.ID)
			}
			claimed, err := Claim(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.host(), f.claim())
			if err != nil || claimed.Start != nil {
				t.Fatalf("revoked command dispatched: %+v, %v", claimed, err)
			}
			got := f.get(t, uuid.UUID(c.ID.Bytes))
			if got.Status != "failed" || got.TerminalReasonCode.String != "permission_revoked" || !got.TerminalAt.Valid || got.StartedAt.Valid {
				t.Fatalf("command not settled before execution: %+v", got)
			}
		})
	}
}

func TestAdmittedCommandRetainsDispatchReceiptAfterRevocation(t *testing.T) {
	f := commandFixture(t)
	first := f.start(t, "admitted")
	f.SetPermissions(t, "sessions.read")
	replay, err := Claim(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.host(), f.claim())
	if err != nil || replay.Start == nil || replay.Start.Command.ID != first.Command.ID {
		t.Fatalf("lost dispatch receipt: %+v %v", replay, err)
	}
	if err := CheckStart(t.Context(), f.Pool, f.host(), *replay.Start); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedCommandSurvivesLogoutButNotAccountRevocation(t *testing.T) {
	for _, incident := range []bool{false, true} {
		t.Run(fmt.Sprint(incident), func(t *testing.T) {
			f := commandFixture(t)
			c, err := Create(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.request("auth-lifetime"))
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.User)
			if incident {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1`, f.User)
			}
			claimed, err := Claim(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.host(), f.claim())
			if err != nil {
				t.Fatal(err)
			}
			if incident {
				if claimed.Start != nil || f.get(t, uuid.UUID(c.ID.Bytes)).Status != "failed" {
					t.Fatal("incident work dispatched", claimed)
				}
			} else if claimed.Start == nil {
				t.Fatal("logout cancelled accepted work")
			}
		})
	}
}
