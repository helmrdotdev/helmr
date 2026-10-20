//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The ordinary workload still owns native answers, saved state and continuation
// assertions. This opt-in adds evidence that its first VM used a qualified seed.
func waitNativeBootSeeds(t *testing.T, ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, evidence string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		var count, ready, failed int
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE b.status='succeeded' AND b.fenced_at IS NOT NULL),count(*) FILTER(WHERE b.status='failed'),
 COALESCE(jsonb_agg(jsonb_build_object('id',b.id,'status',b.status,'error_code',b.error_code,'image_id',b.image_id,'deployment_id',b.deployment_id,'fenced_at',b.fenced_at,'qualification',b.qualification_evidence) ORDER BY b.id),'[]'::jsonb)
 FROM boot_seed_builds b JOIN environments e ON e.id=b.environment_id AND e.current_deployment_id=b.deployment_id
 WHERE b.environment_id=$1 AND b.image_id IS NOT NULL`, env).Scan(&count, &ready, &failed, &raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "boot-seed-preparation.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if failed != 0 {
			t.Fatalf("boot seed qualification failed: %s", raw)
		}
		if count > 0 && count == ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("boot seed qualification incomplete: %s: %v", raw, ctx.Err())
		case <-tick.C:
		}
	}
}

func verifyNativeBootSeedSelection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, sessions map[string]string, evidence, outputName string) {
	t.Helper()
	if len(sessions) < 2 {
		t.Fatal("boot seed proof requires both native Sessions")
	}
	logs, err := os.ReadFile(filepath.Join(evidence, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	samples := make(map[string]json.RawMessage)
	for provider, session := range sessions {
		var selected bool
		var allocationID, seedID string
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT COALESCE(b.status='succeeded' AND b.deployment_id=s.deployment_id AND b.image_id=c.image_id
 AND l.boot_seed_failed_at IS NULL AND l.initialized_at IS NOT NULL,false),
 l.allocation_id::text,COALESCE(l.boot_seed_build_id::text,''),
 jsonb_build_object('session_id',s.id,'computer_id',c.id,'lease_epoch',l.epoch,'boot_seed_build_id',l.boot_seed_build_id,'failed_at',l.boot_seed_failed_at,'initialized_at',l.initialized_at,'deployment_id',s.deployment_id)
 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
 JOIN LATERAL (SELECT * FROM computer_leases x WHERE x.environment_id=c.environment_id AND x.computer_id=c.id ORDER BY x.epoch LIMIT 1) l ON true
 LEFT JOIN boot_seed_builds b ON (b.environment_id,b.id)=(l.environment_id,l.boot_seed_build_id)
 WHERE s.environment_id=$1 AND s.id=$2`, env, session).Scan(&selected, &allocationID, &seedID, &raw)
		if err != nil {
			t.Fatal(err)
		}
		restored := false
		for _, line := range bytes.Split(logs, []byte{'\n'}) {
			var event struct {
				Message    string `json:"msg"`
				Allocation string `json:"allocation_id"`
				Seed       string `json:"boot_seed_id"`
			}
			if json.Unmarshal(line, &event) == nil && event.Message == "Computer boot seed initialized" && event.Allocation == allocationID && event.Seed == seedID && seedID != "" {
				restored = true
				break
			}
		}
		if !restored {
			t.Errorf("%s has no actual seed initialization evidence for allocation %s seed %s", provider, allocationID, seedID)
		}
		samples[provider] = raw
		if !selected {
			t.Errorf("%s did not initialize from its exact Deployment/Image seed: %s", provider, raw)
		}
	}
	raw, err := json.MarshalIndent(samples, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(evidence, outputName), raw, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
