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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// Real independent Computers share one ordinary-GC Worker and remote encrypted
// storage. The enclosing operator samples summed Worker/VMM/cgroup memory.
func verifyNativeConcurrentRestores(t *testing.T, parent context.Context, pool *pgxpool.Pool, c *client.Client, env uuid.UUID, evidence string) {
	t.Helper()
	// The ordinary driver cancels its Sessions, but an empty Computer is not a
	// checkpoint candidate. Explicitly retire that earlier fixture before this
	// independent pair needs both slots.
	retirementCtx, stopRetirement := context.WithTimeout(parent, 5*time.Minute)
	err := retireNativeExecution(retirementCtx, pool, c, env)
	stopRetirement()
	if err != nil {
		t.Fatal("retire previous native workload: ", err)
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	wait := func(check func() (bool, error)) error {
		for {
			ok, err := check()
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	// The earlier native workload must release its physical slot first.
	if err := wait(func() (bool, error) {
		var idle bool
		err := pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM image_builds WHERE environment_id=$1 AND worker_host_id IS NOT NULL AND fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM boot_seed_builds WHERE environment_id=$1 AND ((worker_host_id IS NOT NULL AND fenced_at IS NULL) OR (status='queued' AND image_id IS NOT NULL)))`, env).Scan(&idle)
		return idle, err
	}); err != nil {
		t.Fatal(err)
	}
	var workerID string
	if err := pool.QueryRow(ctx, `SELECT CASE WHEN count(*)=1 THEN min(id::text) END FROM worker_hosts`).Scan(&workerID); err != nil {
		t.Fatal("concurrent proof requires exactly one Worker: ", err)
	}
	input := json.RawMessage(`[{"type":"text","text":"\"recover-check\""}]`)
	type sample struct {
		Session, Computer string
		State             json.RawMessage
	}
	samples := make([]sample, 2)
	// Enclosing retireNativeExecution owns cancellation, convergence and deletion.

	turnResult := func(admission api.TurnAdmission) (json.RawMessage, error) {
		var result json.RawMessage
		err := wait(func() (bool, error) {
			turn, err := c.RetrieveSessionTurn(ctx, admission.SessionID, admission.TurnID, client.EnvironmentScopeOptions{})
			if err != nil {
				return false, err
			}
			switch turn.Status {
			case "completed":
				result = turn.Result
				return true, nil
			case "failed", "cancelled", "interrupted":
				return false, fmt.Errorf("concurrent Turn %s: %s %v", turn.ID, turn.Status, turn.Error)
			}
			return false, nil
		})
		return result, err
	}
	command := func(id, script string) error {
		r, err := c.ExecuteComputer(ctx, id, api.ExecuteComputerRequest{Command: []string{"/bin/sh", "-ceu", script}, Timeout: "1m", IdempotencyKey: uuid.NewV7().String()}, client.ComputerScopeOptions{})
		if err != nil {
			return err
		}
		return wait(func() (bool, error) {
			v, err := c.RetrieveCommand(ctx, r.CommandID, client.ComputerScopeOptions{})
			if err != nil {
				return false, err
			}
			if v.Outcome == nil {
				return false, nil
			}
			if v.Outcome.Kind != "exited" || v.Outcome.ExitCode == nil || *v.Outcome.ExitCode != 0 {
				return false, fmt.Errorf("concurrent command failed: %+v", v.Outcome)
			}
			return true, nil
		})
	}
	var group errgroup.Group
	for i := range samples {
		group.Go(func() error {
			r, err := c.CreateSession(ctx, api.CreateSessionRequest{Agent: "peer", Input: input, IdempotencyKey: fmt.Sprint("concurrent-restore-", i)}, client.EnvironmentScopeOptions{})
			if err != nil {
				return err
			}
			samples[i].Session = r.SessionID
			if r.Turn == nil {
				return fmt.Errorf("missing initial Turn")
			}
			state, err := turnResult(*r.Turn)
			if err != nil {
				return err
			}
			s, err := c.RetrieveSession(ctx, r.SessionID, client.EnvironmentScopeOptions{})
			if err != nil {
				return err
			}
			if s.ComputerID == nil {
				return fmt.Errorf("missing Computer")
			}
			samples[i].Computer = *s.ComputerID
			samples[i].State = state
			return command(*s.ComputerID, fmt.Sprintf(`test ! -e /workspace/concurrent-proof; printf %%s %s:0 > /workspace/concurrent-proof`, *s.ComputerID))
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	ids := []string{samples[0].Computer, samples[1].Computer}
	if ids[0] == ids[1] {
		t.Fatal("concurrency requires independent Computers")
	}
	released := func(epoch int) error {
		return wait(func() (bool, error) {
			var ready bool
			err := pool.QueryRow(ctx, `SELECT count(DISTINCT p.computer_id)=2 FROM checkpoints p JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch) WHERE p.environment_id=$1 AND p.computer_id::text=ANY($2::text[]) AND p.status='ready' AND p.source_lease_epoch=$3 AND l.fenced_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_leases live WHERE live.environment_id=p.environment_id AND live.computer_id=p.computer_id AND live.fenced_at IS NULL)`, env, ids, epoch).Scan(&ready)
			return ready, err
		})
	}
	var observations []json.RawMessage
	for cycle := 1; cycle <= 2; cycle++ {
		if err := released(cycle); err != nil {
			t.Fatal("concurrent release: ", err)
		}
		for i := range samples {
			group.Go(func() error {
				s := samples[i]
				r, err := c.EnqueueSession(ctx, s.Session, api.EnqueueSessionRequest{Input: input, IdempotencyKey: fmt.Sprint("concurrent-cycle-", cycle)}, client.EnvironmentScopeOptions{})
				if err != nil {
					return err
				}
				state, err := turnResult(r)
				if err != nil {
					return err
				}
				var before, after struct {
					Nonce string
					PID   int
				}
				if err = json.Unmarshal(s.State, &before); err != nil {
					return err
				}
				if err = json.Unmarshal(state, &after); err != nil {
					return err
				}
				if before.Nonce == "" || before != after {
					return fmt.Errorf("concurrent restore lost live process: %s -> %s", s.State, state)
				}
				return command(s.Computer, fmt.Sprintf(`test "$(cat /workspace/concurrent-proof)" = %s:%d; printf %%s %s:%d > /workspace/concurrent-proof; sleep 2`, s.Computer, cycle-1, s.Computer, cycle))
			})
		}
		if err := group.Wait(); err != nil {
			t.Fatal(err)
		}
		var valid bool
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(l.worker_host_id::text=$4 AND l.epoch=$3 AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL AND p.status='consumed' AND p.controls_reconciled_at IS NOT NULL),jsonb_agg(jsonb_build_object('worker',l.worker_host_id,'computer',l.computer_id,'epoch',l.epoch,'delivered_at',l.delivered_at,'initialized_at',l.initialized_at,'memory_bytes',l.reserved_memory_bytes,'checkpoint',p.id,'source_epoch',p.source_lease_epoch)) FROM computer_leases l JOIN checkpoints p ON(p.environment_id,p.computer_id,p.target_lease_epoch)=(l.environment_id,l.computer_id,l.epoch) WHERE l.environment_id=$1 AND l.computer_id::text=ANY($2::text[]) AND l.fenced_at IS NULL`, env, ids, cycle+1, workerID).Scan(&valid, &raw)
		if err != nil || !valid {
			t.Fatalf("independent restored owners missing: %s: %v", raw, err)
		}
		observations = append(observations, raw)
	}
	if err := released(3); err != nil {
		t.Fatal("concurrent re-save: ", err)
	}
	// Consume the final save to prove its distinct per-Computer contents,
	// rather than treating a ready metadata row as persistence evidence.
	for _, sample := range samples {
		group.Go(func() error {
			return command(sample.Computer, fmt.Sprintf(`test "$(cat /workspace/concurrent-proof)" = %s:2`, sample.Computer))
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal("final saved contents: ", err)
	}
	logs, err := os.ReadFile(filepath.Join(evidence, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	overlaps, err := nativeHydrationOverlaps(string(logs), ids)
	if err != nil {
		t.Fatal(err)
	}
	var hydrationEvents []string
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "phase=restore_memory_hydration ") && (strings.Contains(line, "computer_id="+ids[0]+" ") || strings.Contains(line, "computer_id="+ids[1]+" ")) {
			// Retain transport/cache counters without the potentially large per-fault array.
			line = regexp.MustCompile(` memory_fault_observations_json=("(?:\\.|[^"\\])*"|[^ ]*)`).ReplaceAllString(line, "")
			hydrationEvents = append(hydrationEvents, line)
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"computers": samples, "restores": observations, "hydration_overlap_lower_bounds": overlaps, "final_resaved_contents_verified": true, "hydration_events": hydrationEvents, "worker": workerID}, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(evidence, "concurrent-restores.json"), raw, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}

	// A separate bounded interval exercises physical preparation beside live
	// interactive work. It does not contribute to the restore timing above.
	cancel()
	ctx, cancel = context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	held, err := c.EnqueueSession(ctx, samples[0].Session, api.EnqueueSessionRequest{
		Input: json.RawMessage(`[{"type":"text","text":"\"hold\""}]`), IdempotencyKey: "hold-during-preparation",
	}, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var heldAfter int64
	if err := wait(func() (bool, error) {
		page, err := c.ReadSessionEvents(ctx, held.SessionID, client.SessionEventReadOptions{After: heldAfter})
		if err != nil {
			return false, err
		}
		heldAfter = page.NextAfter
		for _, event := range page.Records {
			if event.TurnID == nil || *event.TurnID != held.TurnID {
				continue
			}
			switch event.Kind {
			case "turn.output":
				return true, nil
			case "turn.failed", "turn.cancelled", "turn.interrupted":
				return false, fmt.Errorf("preparation peer ended before output: %s", event.Kind)
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelSession(ctx, samples[1].Session, api.CancelSessionRequest{IdempotencyKey: "release-preparation-slot"}, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := wait(func() (bool, error) {
		_, err := c.DeleteComputer(ctx, samples[1].Computer, api.DeleteComputerRequest{IdempotencyKey: "delete-preparation-peer"}, client.ComputerScopeOptions{})
		var apiErr *httpclient.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && apiErr.Code == "computer_busy" {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if err := wait(func() (bool, error) {
		var stopped bool
		err := pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL)`, env, samples[1].Computer).Scan(&stopped)
		return stopped, err
	}); err != nil {
		t.Fatal(err)
	}
	var oldImage, oldBuild string
	if err := pool.QueryRow(ctx, `SELECT image_id::text,image_build_id::text FROM computers WHERE environment_id=$1 AND id=$2`, env, samples[0].Computer).Scan(&oldImage, &oldBuild); err != nil {
		t.Fatal(err)
	}
	refresh, err := c.RefreshTemplate(ctx, "native-continuation", "concurrent-preparation-refresh", client.EnvironmentScopeOptions{})
	if err != nil || refresh.ImageBuildID == oldBuild {
		t.Fatalf("concurrent refresh did not create a new build: %+v: %v", refresh, err)
	}
	var overlap json.RawMessage
	var heldEpoch int64
	if err := wait(func() (bool, error) {
		var active, terminal bool
		var raw []byte
		err := pool.QueryRow(ctx, `SELECT b.started_at IS NOT NULL AND b.fenced_at IS NULL
 AND b.worker_host_id=l.worker_host_id AND c.image_id=$4::uuid AND c.image_build_id=$5::uuid
 AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL,
 b.status IN ('succeeded','failed'),l.epoch,
 jsonb_build_object('observed_at',clock_timestamp(),'build',b.id,'preparation_started_at',b.started_at,'preparation_allocation',b.allocation_id,
 'computer',c.id,'image',c.image_id,'lease_epoch',l.epoch,'computer_allocation',l.allocation_id,'worker',l.worker_host_id,
 'reserved_cpu_millis',b.reserved_cpu_millis+l.reserved_cpu_millis,'reserved_memory_bytes',b.reserved_memory_bytes+l.reserved_memory_bytes)
 FROM image_builds b JOIN computers c ON c.environment_id=b.environment_id AND c.id=$3
 JOIN computer_leases l ON l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL
 WHERE b.environment_id=$1 AND b.id=$2`, env, refresh.ImageBuildID, samples[0].Computer, oldImage, oldBuild).Scan(&active, &terminal, &heldEpoch, &raw)
		if err != nil {
			return false, err
		}
		if terminal {
			return false, fmt.Errorf("preparation ended before physical overlap was observed: %s", raw)
		}
		if !active {
			return false, nil
		}
		paths, err := filepath.Glob("/proc/[0-9]*/comm")
		if err != nil {
			return false, err
		}
		vmCount := 0
		for _, path := range paths {
			name, err := os.ReadFile(path)
			if err == nil && string(name) == "firecracker\n" {
				vmCount++
			}
		}
		if vmCount != 2 {
			return false, nil
		}
		overlap = raw
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := command(samples[0].Computer, fmt.Sprintf(`test "$(cat /workspace/concurrent-proof)" = %s:2`, samples[0].Computer)); err != nil {
		t.Fatal("interactive progress during preparation: ", err)
	}
	var stillActive bool
	var overlapAfter json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT started_at IS NOT NULL AND fenced_at IS NULL,
 jsonb_build_object('observed_at',clock_timestamp(),'build',id,'allocation',allocation_id,'started_at',started_at,'fenced_at',fenced_at)
 FROM image_builds WHERE environment_id=$1 AND id=$2`, env, refresh.ImageBuildID).Scan(&stillActive, &overlapAfter); err != nil || !stillActive {
		t.Fatalf("preparation ended before interactive progress completed: %s: %v", overlapAfter, err)
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
	if vmCount != 2 {
		t.Fatalf("preparation VMM stopped before interactive progress completed: %d VMMs", vmCount)
	}
	refresh, err = c.WaitTemplateBuild(ctx, refresh, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitNativeBootSeeds(t, ctx, pool, env, evidence)
	if _, err := c.SendSessionMessage(ctx, held.SessionID, held.TurnID, api.SessionDataRequest{
		Data: json.RawMessage(`[{"type":"text","text":"\"release\""}]`), IdempotencyKey: "release-after-preparation",
	}, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := turnResult(held)
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		Nonce string
		PID   int
	}
	if err := json.Unmarshal(samples[0].State, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state, &after); err != nil || before.Nonce == "" || before != after {
		t.Fatalf("preparation changed the live process: %s -> %s: %v", samples[0].State, state, err)
	}
	var savedCheckpoint string
	if err := wait(func() (bool, error) {
		err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT p.id::text FROM checkpoints p JOIN computer_leases l
 ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.source_lease_epoch=$3 AND p.status='ready' AND l.fenced_at IS NOT NULL),'')`, env, samples[0].Computer, heldEpoch).Scan(&savedCheckpoint)
		return savedCheckpoint != "", err
	}); err != nil {
		t.Fatal("save after concurrent preparation: ", err)
	}
	if err := command(samples[0].Computer, fmt.Sprintf(`test "$(cat /workspace/concurrent-proof)" = %s:2`, samples[0].Computer)); err != nil {
		t.Fatal("restore after concurrent preparation: ", err)
	}
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT p.status='consumed' AND p.controls_reconciled_at IS NOT NULL
 AND p.source_lease_epoch=$3 AND p.target_lease_epoch>$3 AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL
 FROM checkpoints p JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.target_lease_epoch)
 WHERE p.environment_id=$1 AND p.id=$2`, env, savedCheckpoint, heldEpoch).Scan(&consumed); err != nil || !consumed {
		t.Fatalf("post-preparation checkpoint was not consumed: %s: %v", savedCheckpoint, err)
	}
	raw, err = json.MarshalIndent(map[string]any{"physical_overlap": overlap, "preparation_after_progress": overlapAfter,
		"vmm_count_at_observation": 2, "refresh": refresh, "consumed_checkpoint": savedCheckpoint,
		"live_process_preserved": true, "subsequent_save_consumed": true,
		"scope": "physical preparation and live interactive Computer; concurrent restore timing is recorded separately"}, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(evidence, "concurrent-preparation.json"), raw, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Use the producer's start time and truncated elapsed duration, excluding log
// formatting and delivery delays from the proven hydration interval.
func nativeHydrationOverlaps(logs string, computers []string) ([]time.Duration, error) {
	type interval struct{ start, end time.Time }
	windows := map[string]interval{}
	fields := regexp.MustCompile(`([a-z_]+)=("[^"]*"|[^ ]+)`)
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "phase=restore_memory_hydration ") || len(line) < 19 {
			continue
		}
		values := map[string]string{}
		for _, m := range fields.FindAllStringSubmatch(line, -1) {
			values[m[1]] = strings.Trim(m[2], `"`)
		}
		total, countErr := strconv.Atoi(values["memory_total_chunks"])
		if countErr != nil || total <= 0 || values["error_class"] != "" || values["memory_loaded_chunks"] != values["memory_total_chunks"] {
			continue
		}
		start, err := time.Parse(time.RFC3339Nano, values["phase_started_at"])
		if err != nil {
			return nil, err
		}
		duration, err := strconv.ParseFloat(values["duration_ms"], 64)
		if err != nil {
			return nil, err
		}
		end := start.Add(time.Duration(duration * float64(time.Millisecond)))
		windows[values["computer_id"]+":"+values["lease_epoch"]] = interval{start, end}
	}
	var overlaps []time.Duration
	for epoch := 2; epoch <= 3; epoch++ {
		a, aok := windows[computers[0]+":"+strconv.Itoa(epoch)]
		b, bok := windows[computers[1]+":"+strconv.Itoa(epoch)]
		start, end := a.start, a.end
		if b.start.After(start) {
			start = b.start
		}
		if b.end.Before(end) {
			end = b.end
		}
		if !aok || !bok || !end.After(start) {
			return nil, fmt.Errorf("epoch %d has no proven overlapping complete hydration: %v %v", epoch, a, b)
		}
		overlaps = append(overlaps, end.Sub(start))
	}
	return overlaps, nil
}

func TestNativeHydrationOverlapRejectsSerialAndIncompleteEvidence(t *testing.T) {
	line := func(id string, epoch, second int, loaded string) string {
		return fmt.Sprintf("2026/10/20 15:00:%02d INFO Computer VM phase completed computer_id=%s lease_epoch=%d phase=restore_memory_hydration phase_started_at=2026-10-20T15:00:%02dZ duration_ms=5000 error_class=\"\" memory_loaded_chunks=%s memory_total_chunks=4096\n", second, id, epoch, second-5, loaded)
	}
	for _, tc := range []struct {
		name   string
		second int
		loaded string
		want   bool
	}{{"overlap", 12, "4096", true}, {"serial", 16, "4096", false}, {"incomplete", 12, "4095", false}} {
		t.Run(tc.name, func(t *testing.T) {
			logs := line("a", 2, 10, "4096") + line("b", 2, tc.second, tc.loaded) + line("a", 3, 30, "4096") + line("b", 3, 32, "4096")
			got, err := nativeHydrationOverlaps(logs, []string{"a", "b"})
			if (err == nil) != tc.want {
				t.Fatalf("overlaps=%v err=%v", got, err)
			}
			if tc.want && (len(got) != 2 || got[0] != 3*time.Second) {
				t.Fatalf("imprecise lower bound: %v", got)
			}
		})
	}
}
