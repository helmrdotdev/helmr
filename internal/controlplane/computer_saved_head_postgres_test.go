package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

// Models a committed save with the same disk contents. Save admission/upload is
// is outside this fixture; it tests consumers of an advanced recovery head.
func advanceActorSavedHead(t *testing.T, f *actorCheckpointFixture) uuid.UUID {
	t.Helper()
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_versions(id,environment_id,workspace_id,parent_version_id,content_digest,size_bytes,entry_count,status,source_workspace_lease_id,ownership_generation,writer_generation,published_at)
 SELECT $2,v.environment_id,v.workspace_id,v.id,v.content_digest,v.size_bytes,v.entry_count,'committed',$3,c.ownership_generation,c.writer_generation,now()
 FROM computers c JOIN computer_versions v ON v.id=c.head_version_id WHERE c.id=$1`, f.workspaceID, next, f.claim.workspaceLease.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator)
 SELECT r.environment_id,r.computer_id,$2,r.locator FROM computers c JOIN computer_version_roots r ON r.version_id=c.head_version_id WHERE c.id=$1`, f.workspaceID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET head_version_id=$2,revision=revision+1 WHERE id=$1`, f.workspaceID, next)
	return next
}

func TestActorCompletionAndCheckpointIgnoreSavedHeadAdvancement(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "resident", true: "restored"}[restored], func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			content := f.capture(t, "retained computer")
			f.turn(t, 1)
			advanceActorSavedHead(t, f)
			if restored {
				f.suspend(t, content)
				if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"prompt":"continue"}`)}); err != nil {
					t.Fatal(err)
				}
				f.placeAndStart(t)
				f.turn(t, 2)
			}
			predecessor := advanceActorSavedHead(t, f)
			f.close(t)
			sequence := int64(1)
			if restored {
				sequence = 2
			}
			f.complete(t, sequence, content)
			var origin, parent uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT a.base_workspace_version_id,v.parent_version_id FROM run_attempts a JOIN computers c ON c.id=a.workspace_id JOIN computer_versions v ON v.id=c.head_version_id WHERE a.run_id=$1 AND a.number=1`, f.runID).Scan(&origin, &parent); err != nil {
				t.Fatal(err)
			}
			if origin != f.rootID || parent != predecessor {
				t.Fatalf("origin=%s predecessor=%s; expected %s / %s", origin, parent, f.rootID, predecessor)
			}
		})
	}
}
