package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionStartRequiresCurrentSetupAuthority(t *testing.T) {
	for _, mode := range []string{"current", "stale attachment", "held", "cancelled", "revoked", "ready", "stopping", "failed", "expired", "acquiring", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			f, _, e := startingSessionFixture(t)
			attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
			if err != nil {
				t.Fatal(err)
			}
			sequence := attachment.Sequence
			switch mode {
			case "stale attachment":
				sequence++
			case "held":
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,gen_random_uuid(),$2,'local','test')`, f.env, e.SessionID)
			case "cancelled":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.env, e.SessionID)
			case "revoked":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
			case "ready", "stopping":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status=$3 WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID, mode)
			case "failed":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET failure_recorded_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, e.SessionID)
			case "expired":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.env)
			case "acquiring":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='acquiring' WHERE environment_id=$1`, f.env)
			case "deleted":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1`, f.env)
			}
			start, err := AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, sequence)
			if mode == "current" {
				if err != nil || start.BundleDigest == "" || start.AgentKey == "" || start.Authority.Generation != 1 || !start.Authority.ExpiresAt.After(time.Now()) {
					t.Fatalf("start: %+v %v", start, err)
				}
				again, err := AuthorizeSessionStart(t.Context(), f.pool, *f.host(), e, sequence)
				if err != nil || again.BundleDigest != start.BundleDigest || again.DeploymentID != start.DeploymentID {
					t.Fatalf("retry: %+v %v", again, err)
				}
			} else if !errors.Is(err, ErrNotReady) {
				t.Fatalf("invalid start: %+v %v", start, err)
			}
			if mode == "held" || mode == "cancelled" || mode == "revoked" || mode == "stopping" {
				if _, err := RenewRuntimeAuthority(t.Context(), f.pool, *f.host(), e); err != nil {
					t.Fatalf("control still required: %v", err)
				}
			}
		})
	}
}
