//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Refresh through the public API, then observe useful prepared work on the first
// and second fresh Computer in that generation. Caches remain under their normal
// policy; "second" is a reuse opportunity, not a claim that every byte is cached.
func verifyNativeSeedGeneration(t *testing.T, parent context.Context, pool *pgxpool.Pool, c *client.Client, env uuid.UUID, oldComputer, evidence string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	wait := func(check func() (bool, error)) {
		t.Helper()
		for {
			ok, err := check()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	idle := func() (bool, error) {
		var ready bool
		err := pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL)`, env).Scan(&ready)
		return ready, err
	}
	waitTurn := func(session, turnID string) {
		t.Helper()
		wait(func() (bool, error) {
			turn, err := c.RetrieveSessionTurn(ctx, session, turnID, client.EnvironmentScopeOptions{})
			if err != nil {
				return false, err
			}
			switch turn.Status {
			case "completed":
				return true, nil
			case "failed", "cancelled", "interrupted":
				return false, fmt.Errorf("generation Turn %s ended %s", turnID, turn.Status)
			default:
				return false, nil
			}
		})
	}
	// A ready-image selection need not attach a Computer build waiter.
	// Resolve the generation through its immutable image pin.
	var oldImage, oldBuild string
	var cpuMillis, memoryBytes int64
	if err := pool.QueryRow(ctx, `SELECT c.image_id::text,i.image_build_id::text,c.cpu_millis,c.memory_bytes FROM computers c JOIN images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id) WHERE c.environment_id=$1 AND c.id=$2`, env, oldComputer).Scan(&oldImage, &oldBuild, &cpuMillis, &memoryBytes); err != nil {
		t.Fatal(err)
	}
	// Keep interactive work active while refresh competes for the single slot.
	// Preparation must wait without blocking that work or changing its image pin.
	keeper, err := c.CreateSession(ctx, api.CreateSessionRequest{Agent: "peer", ComputerID: oldComputer, Input: json.RawMessage(`[{"type":"text","text":"\"hold\""}]`), IdempotencyKey: "old-generation-pin"}, client.EnvironmentScopeOptions{})
	if err != nil || keeper.Turn == nil {
		t.Fatalf("retain existing generation: %v", err)
	}
	var keeperAfter int64
	wait(func() (bool, error) {
		page, err := c.ReadSessionEvents(ctx, keeper.SessionID, client.SessionEventReadOptions{After: keeperAfter})
		if err != nil {
			return false, err
		}
		keeperAfter = page.NextAfter
		for _, event := range page.Records {
			if event.TurnID == nil || *event.TurnID != keeper.Turn.TurnID {
				continue
			}
			switch event.Kind {
			case "turn.output":
				return true, nil
			case "turn.failed", "turn.cancelled", "turn.interrupted":
				return false, fmt.Errorf("generation keeper ended before output: %s", event.Kind)
			}
		}
		return false, nil
	})
	refreshAt := time.Now()
	build, err := c.RefreshTemplate(ctx, "native-continuation", "seed-generation-refresh", client.EnvironmentScopeOptions{})
	if err != nil || build.ImageBuildID == oldBuild {
		t.Fatalf("refresh did not request a fresh generation: %+v: %v", build, err)
	}
	var contention []json.RawMessage
	observeQueued := func() {
		t.Helper()
		var valid bool
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT b.status='queued' AND b.worker_host_id IS NULL
 AND c.image_id=$4::uuid AND i.image_build_id=$5::uuid
 AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL
 AND (SELECT count(*) FROM computer_leases x WHERE x.environment_id=$1 AND x.fenced_at IS NULL)=1
 AND NOT EXISTS(SELECT 1 FROM image_builds x WHERE x.environment_id=$1 AND x.worker_host_id IS NOT NULL AND x.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM boot_seed_builds x WHERE x.environment_id=$1 AND x.worker_host_id IS NOT NULL AND x.fenced_at IS NULL),
 jsonb_build_object('observed_at',clock_timestamp(),'build',b.id,'status',b.status,'computer',c.id,'image',c.image_id,'lease_epoch',l.epoch,'allocation',l.allocation_id)
 FROM image_builds b JOIN computers c ON c.environment_id=b.environment_id AND c.id=$3
 JOIN images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id)
 JOIN computer_leases l ON l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL
 WHERE b.environment_id=$1 AND b.id=$2`, env, build.ImageBuildID, oldComputer, oldImage, oldBuild).Scan(&valid, &raw)
		if err != nil || !valid {
			t.Fatalf("preparation did not wait behind live pinned work: %s: %v", raw, err)
		}
		paths, err := filepath.Glob("/proc/[0-9]*/comm")
		if err != nil {
			t.Fatal(err)
		}
		vmCount := 0
		for _, path := range paths {
			name, err := os.ReadFile(path)
			if err == nil && string(name) == "firecracker\n" {
				vmCount++
			}
		}
		if vmCount != 1 {
			t.Fatalf("queued preparation admitted extra physical work: %d VMMs", vmCount)
		}
		contention = append(contention, raw)
	}
	observeQueued()
	progressAt := time.Now()
	command, err := c.ExecuteComputer(ctx, oldComputer, api.ExecuteComputerRequest{
		Command: []string{"/bin/sh", "-ceu", "test -s /workspace/peer-" + keeper.SessionID + ".json && test -f /workspace/prepared-environment"},
		Timeout: "1m", IdempotencyKey: "progress-during-preparation",
	}, client.ComputerScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait(func() (bool, error) {
		result, err := c.RetrieveCommand(ctx, command.CommandID, client.ComputerScopeOptions{})
		if err != nil || result.Outcome == nil {
			return false, err
		}
		if result.Outcome.Kind != "exited" || result.Outcome.ExitCode == nil || *result.Outcome.ExitCode != 0 {
			return false, fmt.Errorf("live work stalled behind queued preparation: %+v", result.Outcome)
		}
		return true, nil
	})
	progressMS := time.Since(progressAt).Seconds() * 1000
	observeQueued()
	if _, err := c.SendSessionMessage(ctx, keeper.SessionID, keeper.Turn.TurnID, api.SessionDataRequest{Data: json.RawMessage(`[{"type":"text","text":"\"release\""}]`), IdempotencyKey: "old-generation-release"}, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	waitTurn(keeper.SessionID, keeper.Turn.TurnID)
	wait(idle)
	build, err = c.WaitTemplateBuild(ctx, build, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var image, seed string
	wait(func() (bool, error) {
		var ready, failed bool
		var count int
		var failure string
		err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(bool_and(b.status='succeeded' AND b.fenced_at IS NOT NULL AND b.payload_retired_at IS NULL),false),
 COALESCE(bool_or(b.status='failed'),false),COALESCE(min(b.error_code),''),COALESCE(min(i.id::text),''),COALESCE(min(b.id::text),'')
 FROM images i JOIN boot_seed_builds b ON(b.environment_id,b.image_id)=(i.environment_id,i.id)
 JOIN environments e ON e.id=b.environment_id AND e.current_deployment_id=b.deployment_id
 WHERE i.environment_id=$1 AND i.image_build_id=$2 AND b.deployment_id=$3 AND b.cpu_millis=$4 AND b.memory_bytes=$5`, env, build.ImageBuildID, build.DeploymentID, cpuMillis, memoryBytes).Scan(&count, &ready, &failed, &failure, &image, &seed)
		if err == nil && (count > 1 || failed) {
			return false, fmt.Errorf("generation seed invariant: matching=%d failed=%v code=%s", count, failed, failure)
		}
		return ready, err
	})
	if image == oldImage {
		t.Fatal("refresh reused the old image identity")
	}
	preparationMS := time.Since(refreshAt).Seconds() * 1000
	sessions := make(map[string]string)
	nonces := make(map[string]bool)
	var samples []map[string]any
	for _, phase := range []string{"first-after-refresh", "second-after-refresh"} {
		started := time.Now()
		r, err := c.CreateSession(ctx, api.CreateSessionRequest{Agent: "peer", Input: json.RawMessage(`[{"type":"text","text":"\"hold-performance\""}]`), IdempotencyKey: phase}, client.EnvironmentScopeOptions{})
		if err != nil || r.Turn == nil {
			t.Fatalf("fresh generation Session: %v", err)
		}
		ackMS := time.Since(started).Seconds() * 1000
		var output api.AgentSessionEvent
		var after int64
		wait(func() (bool, error) {
			page, err := c.ReadSessionEvents(ctx, r.SessionID, client.SessionEventReadOptions{After: after})
			if err != nil {
				return false, err
			}
			after = page.NextAfter
			for _, event := range page.Records {
				if event.TurnID == nil || *event.TurnID != r.Turn.TurnID {
					continue
				}
				switch event.Kind {
				case "turn.output":
					output = event
					return true, nil
				case "turn.failed", "turn.cancelled", "turn.interrupted":
					return false, fmt.Errorf("generation Turn ended before output: %s", event.Kind)
				}
			}
			return false, nil
		})
		firstOutputMS := time.Since(started).Seconds() * 1000
		var content []struct{ Type, Text string }
		var work struct {
			Nonce        string
			PreparedWork struct {
				Kind string
			}
		}
		if err := json.Unmarshal(output.Data, &content); err != nil || len(content) != 1 || content[0].Type != "text" {
			t.Fatalf("unexpected prepared output: %s: %v", output.Data, err)
		}
		if err := json.Unmarshal([]byte(content[0].Text), &work); err != nil || work.PreparedWork.Kind != "edit-test" || work.Nonce == "" || nonces[work.Nonce] {
			t.Fatalf("missing prepared work or fresh init identity: %s: %v", output.Data, err)
		}
		nonces[work.Nonce] = true
		var computer string
		var valid bool
		err = pool.QueryRow(ctx, `SELECT c.id::text,c.image_id=$3::uuid AND i.image_build_id=$4::uuid AND l.boot_seed_build_id=$5::uuid
 AND l.initialized_at IS NOT NULL AND l.boot_seed_failed_at IS NULL AND l.fenced_at IS NULL
 AND old.image_id=$6::uuid AND old_image.image_build_id=$7::uuid AND old.deleted_at IS NULL
 FROM sessions s JOIN computers c ON(c.environment_id,c.id)=(s.environment_id,s.computer_id)
 JOIN images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id)
 JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.id,1)
 JOIN computers old ON old.environment_id=c.environment_id AND old.id=$8
 JOIN images old_image ON (old_image.environment_id,old_image.id)=(old.environment_id,old.image_id)
 WHERE s.environment_id=$1 AND s.id=$2`, env, r.SessionID, image, build.ImageBuildID, seed, oldImage, oldBuild, oldComputer).Scan(&computer, &valid)
		if err != nil || !valid {
			t.Fatalf("fresh generation first lease is not initialized on the new seed, or the old pin changed: %v", err)
		}
		sessions[phase] = r.SessionID
		samples = append(samples, map[string]any{"phase": phase, "session": r.SessionID, "computer": computer, "request_started_at": started, "ack_ms": ackMS, "first_output_observed_ms": firstOutputMS, "output": output})
		if _, err := c.SendSessionMessage(ctx, r.SessionID, r.Turn.TurnID, api.SessionDataRequest{Data: json.RawMessage(`[{"type":"text","text":"\"release\""}]`), IdempotencyKey: phase + "-release"}, client.EnvironmentScopeOptions{}); err != nil {
			t.Fatal(err)
		}
		waitTurn(r.SessionID, r.Turn.TurnID)
		if _, err := c.CancelSession(ctx, r.SessionID, api.CancelSessionRequest{IdempotencyKey: phase + "-cancel"}, client.EnvironmentScopeOptions{}); err != nil {
			t.Fatal(err)
		}
		wait(func() (bool, error) {
			_, err := c.DeleteComputer(ctx, computer, api.DeleteComputerRequest{IdempotencyKey: phase + "-delete"}, client.ComputerScopeOptions{})
			var apiErr *httpclient.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && apiErr.Code == "computer_busy" {
				return false, nil
			}
			return err == nil, err
		})
		wait(idle)
	}
	// This also requires the Worker's actual successful seed-initialization log,
	// rather than treating the control-plane selection as proof of VM behavior.
	verifyNativeBootSeedSelection(t, ctx, pool, env, sessions, evidence, "boot-seed-generation-selection.json")
	raw, err := json.Marshal(map[string]any{"old_image": oldImage, "image": image, "seed": seed, "build": build, "refresh_request_to_qualified_seed_ms_including_contention": preparationMS, "samples": samples,
		"concurrent_preparation_demand": map[string]any{"queued_observations": contention, "progress_command": command.CommandID, "progress_command_ms": progressMS, "scope": "queued preparation under saturated single-slot capacity; physical preparation starts after interactive release"}})
	if err == nil {
		err = os.WriteFile(filepath.Join(evidence, "seed-generation.json"), raw, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
