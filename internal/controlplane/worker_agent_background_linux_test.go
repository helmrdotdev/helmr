//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestWorkerAgentBackgroundSaves(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionBackground)
}

// Publication is observed after the real Control Plane commits, while the peer
// Turn is still running. The driver waits for three successive optional saves
// before starting native Turns, then retains the normal own-save assertions.
func (g *nativePublicationGate) observeBackgroundPublication(ctx context.Context, save string) error {
	g.mu.Lock()
	done := len(g.background) >= 3
	g.mu.Unlock()
	if done {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var valid bool
	var observation json.RawMessage
	err := g.pool.QueryRow(ctx, `SELECT
 c.recovery_save_id=s.id AND s.status='published' AND s.root_id IS NOT NULL
 AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()
 AND t.completion_save_id IS NULL,
 jsonb_build_object('saveId',s.id,'computerId',s.computer_id,'leaseEpoch',s.computer_lease_epoch,
 'sequence',s.seq,'peerSessionId',p.id,'peerTurnId',t.id,
 'requestedAt',s.requested_at,'capturedAt',s.captured_at,'observedAt',clock_timestamp(),
 'peerCounter',(SELECT COALESCE(max(((convert_from(e.data,'UTF8')::jsonb->0->>'text')::jsonb->>'count')::bigint),0)
  FROM session_events e WHERE e.environment_id=t.environment_id AND e.turn_id=t.id AND e.kind='turn.output'),
 'pendingSaves',(SELECT count(*) FROM computer_saves pending WHERE pending.environment_id=s.environment_id AND pending.computer_id=s.computer_id AND pending.status IN ('requested','captured')),
 'retainedSaves',(SELECT count(*) FROM computer_saves retained WHERE retained.environment_id=s.environment_id AND retained.computer_id=s.computer_id AND retained.root_id IS NOT NULL),
 'retainedRoots',(SELECT count(DISTINCT retained.root_id) FROM computer_saves retained WHERE retained.environment_id=s.environment_id AND retained.computer_id=s.computer_id AND retained.root_id IS NOT NULL),
 'pinnedBytes',(SELECT COALESCE(sum(o.size_bytes),0) FROM computer_objects o WHERE o.environment_id=s.environment_id AND EXISTS(
  SELECT 1 FROM computer_object_pins pin JOIN computer_saves retained ON (retained.environment_id,retained.id)=(pin.environment_id,pin.save_id)
  WHERE pin.environment_id=o.environment_id AND pin.digest=o.digest AND retained.computer_id=s.computer_id)))
 FROM computer_saves s
 JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 JOIN sessions p ON (p.environment_id,p.computer_id)=(s.environment_id,s.computer_id)
 JOIN agents a ON (a.environment_id,a.id)=(p.environment_id,p.agent_id) AND a.name='peer'
 JOIN turns t ON (t.environment_id,t.session_id)=(p.environment_id,p.id) AND t.status='running'
 WHERE s.environment_id=$1 AND s.id=$2 AND s.turn_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=s.environment_id AND cp.disk_save_id=s.id)`, g.environment, save).Scan(&valid, &observation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("background publication did not advance a live recovery head independently of Turn completion")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var identity struct {
		SaveID string `json:"saveId"`
	}
	for _, old := range g.background {
		if err := json.Unmarshal(old, &identity); err != nil {
			return err
		}
		if identity.SaveID == save {
			return nil
		}
	}
	g.background = append(g.background, observation)
	encoded, err := json.Marshal(g.background)
	if err != nil {
		return err
	}
	path := filepath.Join(g.evidence, "background-publications.json")
	if err := os.WriteFile(path+".new", encoded, 0600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}
