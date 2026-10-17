//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativePerformanceConfig struct {
	Kind     string
	Degraded bool
}

func TestWorkerAgentPerformance(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionPerformance)
}

func TestNativePerformanceGateForwardsUnselectedSegmentWithBinaryDigest(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	object := workerapi.AgentSaveObject{
		Save:        workerapi.AgentSave{SaveID: uuid.NewV7().String()},
		Inspections: []blockformat.ObjectInspection{{Segment: &blockformat.Ref{Digest: [32]byte{0xff, 0x00, 0xdc}}}},
	}
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	gate := nativePerformanceGate{pool: database.Pool, environment: uuid.NewV7(), next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("forwarded body changed: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	for range 2 {
		response := httptest.NewRecorder()
		gate.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/objects/certify", bytes.NewReader(body)))
		if response.Code != http.StatusNoContent {
			t.Fatalf("unselected segment was blocked: %d %s", response.Code, response.Body.String())
		}
	}
	if forwarded != 2 || !gate.nonNative[object.Save.SaveID] {
		t.Fatal("non-native save was not forwarded and cached")
	}
	// A transient observer query failure must invalidate the sample even when a
	// later retry forwards successfully; its delay is not product latency.
	failedGate := nativePerformanceGate{pool: database.Pool, environment: gate.environment, next: gate.next}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/objects/certify", bytes.NewReader(body)).WithContext(cancelled)
	failedGate.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("cancelled observation returned %d", response.Code)
	}
	response = httptest.NewRecorder()
	failedGate.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/objects/certify", bytes.NewReader(body)))
	if response.Code != http.StatusNoContent || failedGate.verify(nil) == nil {
		t.Fatal("successful retry erased the observer failure")
	}
	directory := t.TempDir()
	if err := failedGate.retain(filepath.Join(directory, "performance-storage-windows.json")); err != nil {
		t.Fatal(err)
	}
	failures, err := os.ReadFile(filepath.Join(directory, "performance-observer-errors.json"))
	if err != nil || !bytes.Contains(failures, []byte("classify certification request")) {
		t.Fatalf("observer failure was not retained: %v %s", err, failures)
	}
}

type nativePerformanceAttempt struct {
	SaveID, Digest, StartedAt, FaultStartedAt, FaultEndsAt string
	DurationMS                                             float64
	Status                                                 int
	Injected                                               bool
}

type nativePerformanceGate struct {
	pool           *pgxpool.Pool
	environment    uuid.UUID
	next           http.Handler
	degraded       bool
	mu             sync.Mutex
	nonNative      map[string]bool
	selected       map[string]string
	attempts       []nativePerformanceAttempt
	observerErrors []string
}

func (g *nativePerformanceGate) observerFailed(stage, save string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observerErrors = append(g.observerErrors, fmt.Sprintf("%s stage=%s save=%s error=%v", time.Now().UTC().Format(time.RFC3339Nano), stage, save, err))
}

func (g *nativePerformanceGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/worker/v1/computer-saves/objects/certify" {
		g.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		g.observerFailed("read certification request", "", err)
		http.Error(w, "performance observation read failed", http.StatusInternalServerError)
		return
	}
	// Preserve a larger non-segment request exactly; the observer's decode bound
	// must not become a new production request limit.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	var object workerapi.AgentSaveObject
	if json.Unmarshal(body, &object) != nil {
		g.next.ServeHTTP(w, r)
		return
	}
	save := object.Save.SaveID
	g.mu.Lock()
	selected := g.selected[save]
	nonNative := g.nonNative[save]
	g.mu.Unlock()
	candidate, found := nativeBatchObject(object.Inspections, selected, true)
	if !found {
		g.next.ServeHTTP(w, r)
		return
	}
	digest := candidate.Digest
	if nonNative {
		g.next.ServeHTTP(w, r)
		return
	}
	if selected == "" {
		var native, uncertified bool
		err := g.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM computer_saves s
 JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 JOIN sessions se ON (se.environment_id,se.id)=(t.environment_id,t.session_id)
 JOIN agents a ON (a.environment_id,a.id)=(se.environment_id,se.agent_id)
 WHERE s.environment_id=$1 AND s.id=$2 AND a.key IN ('codex','claude')
), EXISTS(SELECT 1 FROM disk_objects o WHERE o.environment_id=$1 AND o.digest=$3 AND NOT o.certified)`, g.environment, save, digest).Scan(&native, &uncertified)
		if err != nil {
			g.observerFailed("classify certification request", save, err)
			http.Error(w, "performance observation query failed", http.StatusInternalServerError)
			return
		}
		if !native {
			g.mu.Lock()
			if g.nonNative == nil {
				g.nonNative = map[string]bool{}
			}
			g.nonNative[save] = true
			g.mu.Unlock()
		}
		if !native || !uncertified {
			g.next.ServeHTTP(w, r)
			return
		}
		g.mu.Lock()
		if g.selected == nil {
			g.selected = map[string]string{}
		}
		if g.selected[save] == "" {
			g.selected[save] = digest
		}
		selected = g.selected[save]
		g.mu.Unlock()
	}
	if selected != digest {
		g.next.ServeHTTP(w, r)
		return
	}
	if g.degraded {
		r.Header.Set(nativeStatFaultHeader, digest)
		r.Header.Set(nativeStatWindowHeader, save)
	}
	started := time.Now()
	response := httptest.NewRecorder()
	g.next.ServeHTTP(response, r)
	attempt := nativePerformanceAttempt{SaveID: save, Digest: digest, StartedAt: started.UTC().Format(time.RFC3339Nano),
		DurationMS: float64(time.Since(started)) / float64(time.Millisecond), Status: response.Code,
		Injected:       response.Header().Get(nativeStatFaultHeader) == "observed",
		FaultStartedAt: response.Header().Get(nativeStatWindowHeader + "-Start"), FaultEndsAt: response.Header().Get(nativeStatWindowHeader + "-End")}
	g.mu.Lock()
	g.attempts = append(g.attempts, attempt)
	g.mu.Unlock()
	for key, values := range response.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(response.Code)
	_, _ = w.Write(response.Body.Bytes())
}

func (g *nativePerformanceGate) retain(path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	failures, err := json.MarshalIndent(g.observerErrors, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "performance-observer-errors.json"), failures, 0600); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g.attempts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (g *nativePerformanceGate) verify(saveIDs []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.observerErrors) != 0 {
		return fmt.Errorf("performance observer failed: %v", g.observerErrors)
	}
	if !g.degraded {
		// Turn coverage may reuse certified objects or coalesce with another
		// save. Validate observed storage work independently of that coverage.
		if len(g.selected) == 0 {
			return errors.New("healthy storage case selected no native certification")
		}
		saveIDs = make([]string, 0, len(g.selected))
		for save := range g.selected {
			saveIDs = append(saveIDs, save)
		}
	}
	for _, save := range saveIDs {
		injected, recovered := false, false
		start, end := "", ""
		for _, attempt := range g.attempts {
			if attempt.SaveID != save {
				continue
			}
			if attempt.Digest != g.selected[save] {
				return errors.New("performance retry changed object identity")
			}
			if attempt.Injected {
				if attempt.Status != http.StatusServiceUnavailable || attempt.FaultStartedAt == "" || attempt.FaultEndsAt == "" {
					return fmt.Errorf("invalid storage-unavailable receipt for %s", save)
				}
				if injected && (start != attempt.FaultStartedAt || end != attempt.FaultEndsAt) {
					return errors.New("storage unavailability restarted its deadline")
				}
				start, end, injected = attempt.FaultStartedAt, attempt.FaultEndsAt, true
			} else if attempt.Status >= 200 && attempt.Status < 300 {
				if g.degraded && (!injected || attempt.FaultStartedAt != start || attempt.FaultEndsAt != end) {
					return fmt.Errorf("storage retry did not preserve its window for %s", save)
				}
				recovered = true
			}
		}
		if injected != g.degraded || !recovered {
			return fmt.Errorf("storage case was not exercised for %s: injected=%v recovered=%v", save, injected, recovered)
		}
	}
	return nil
}

func TestNativePerformanceGateHealthyCoverageCanReuseObjects(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		degraded, empty, injected, wrongDigest bool
		status                                 int
		wantError                              bool
	}{
		{name: "shared or reused coverage", status: http.StatusNoContent},
		{name: "no selected storage", empty: true, wantError: true},
		{name: "selected storage did not succeed", status: http.StatusServiceUnavailable, wantError: true},
		{name: "changed object", status: http.StatusNoContent, wrongDigest: true, wantError: true},
		{name: "unexpected injection", status: http.StatusServiceUnavailable, injected: true, wantError: true},
		{name: "degraded still requires each save", degraded: true, status: http.StatusNoContent, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := nativePerformanceGate{degraded: tc.degraded}
			if !tc.empty {
				gate.selected = map[string]string{"selected": "digest"}
				digest := "digest"
				if tc.wrongDigest {
					digest = "other"
				}
				gate.attempts = []nativePerformanceAttempt{{SaveID: "selected", Digest: digest,
					Status: tc.status, Injected: tc.injected}}
				if tc.degraded {
					gate.attempts = []nativePerformanceAttempt{
						{SaveID: "selected", Digest: digest, Status: http.StatusServiceUnavailable, Injected: true, FaultStartedAt: "start", FaultEndsAt: "end"},
						{SaveID: "selected", Digest: digest, Status: http.StatusNoContent, FaultStartedAt: "start", FaultEndsAt: "end"},
					}
					if err := gate.verify([]string{"selected"}); err != nil {
						t.Fatalf("selected degraded storage was not valid: %v", err)
					}
				}
			}
			if err := gate.verify([]string{"no-new-certification", "selected"}); (err != nil) != tc.wantError {
				t.Fatalf("verify error = %v, wantError = %v", err, tc.wantError)
			}
		})
	}
}

func TestNativeStorageFaultWindowStartsAtFirstUseAndDoesNotExtend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window := &nativeStatWindow{}
		time.Sleep(time.Second)
		first := httptest.NewRecorder()
		if !window.unavailable(first) {
			t.Fatal("first storage call was not unavailable")
		}
		time.Sleep(time.Second)
		second := httptest.NewRecorder()
		if !window.unavailable(second) || first.Header().Get(nativeStatWindowHeader+"-Start") != second.Header().Get(nativeStatWindowHeader+"-Start") {
			t.Fatal("retry did not retain the first storage call")
		}
		time.Sleep(time.Second)
		if window.unavailable(httptest.NewRecorder()) {
			t.Fatal("storage outage extended beyond its fixed window")
		}
	})
}

type nativeTurnTiming struct {
	Source, Event, SessionID, TurnID                           string
	Sequence                                                   int64
	Outcome                                                    string
	HandlerReturnToSettlementMS, HandlerReturnToFinalizationMS *float64
	FinalizationToSettlementMS                                 *float64
}

func TestNativePerformancePublicationCancellationRetainsUnknown(t *testing.T) {
	// Pool creation is lazy. A canceled wait must not need a running database
	// in order to preserve its deadline and failed-observation metadata.
	pool, err := pgxpool.New(t.Context(), "postgres://localhost:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	input := nativeExecutionConfig{Evidence: t.TempDir(), Performance: &nativePerformanceConfig{Kind: "dependencies"}}
	err = awaitNativePerformancePublications(ctx, pool, uuid.NewV7(), []string{uuid.NewV7().String()}, input)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(input.Evidence, "performance-publication-wait.json"))
	if err != nil {
		t.Fatal(err)
	}
	var observations []struct {
		Stage, WaitError, ObservationError string
		PendingTurnIDs                     json.RawMessage
		EffectiveDeadline                  time.Time
	}
	if err := json.Unmarshal(data, &observations); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Stage != "initial-30s" || observations[0].WaitError == "" ||
		observations[0].ObservationError == "" || string(observations[0].PendingTurnIDs) != "null" || observations[0].EffectiveDeadline.IsZero() {
		t.Fatalf("canceled collection lost unknown-state evidence: %s", data)
	}
}

// The dependency workload changes hundreds of MiB. Its final-save collection
// budget is separate from small-delta latency targets and production timeouts.
// Keep a thirty-second observation even when collecting later convergence.
func awaitNativePerformancePublications(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, turns []string, input nativeExecutionConfig) error {
	started := time.Now()
	budget := 30 * time.Second
	if input.Performance.Kind == "dependencies" {
		budget = 5 * time.Minute
	}
	collectionCtx, cancelCollection := context.WithDeadline(ctx, started.Add(budget))
	defer cancelCollection()
	deadline, _ := collectionCtx.Deadline()
	observations := []json.RawMessage{}
	observe := func(stage string, waitErr error) error {
		var pending []string
		observationErr := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(t.id::text ORDER BY t.id),ARRAY[]::text[])
 FROM turns t LEFT JOIN computer_saves requested ON (requested.environment_id,requested.turn_id)=(t.environment_id,t.id)
 LEFT JOIN computer_saves covering ON (covering.environment_id,covering.id)=(requested.environment_id,COALESCE(requested.covered_by_save_id,requested.id))
 WHERE t.environment_id=$1 AND t.id=ANY($2::uuid[]) AND covering.status IS DISTINCT FROM 'published'`, env, turns).Scan(&pending)
		observationError := ""
		if observationErr != nil {
			pending = nil // Unknown is not evidence that no publications remain.
			observationError = observationErr.Error()
		} else if pending == nil {
			pending = []string{}
		}
		errorText := ""
		if waitErr != nil {
			errorText = waitErr.Error()
		}
		data, err := json.Marshal(map[string]any{"stage": stage, "startedAt": started.UTC(), "observedAt": time.Now().UTC(),
			"elapsedMS": float64(time.Since(started)) / float64(time.Millisecond), "collectionBudgetMS": budget.Milliseconds(),
			"effectiveDeadline": deadline.UTC(), "pendingTurnIDs": pending, "expectedTurns": len(turns), "waitError": errorText,
			"observationError": observationError})
		if err != nil {
			return err
		}
		observations = append(observations, data)
		data, err = json.MarshalIndent(observations, "", "  ")
		if err != nil {
			return err
		}
		return errors.Join(observationErr, os.WriteFile(filepath.Join(input.Evidence, "performance-publication-wait.json"), data, 0600))
	}
	initialCtx, cancelInitial := context.WithTimeout(collectionCtx, 30*time.Second)
	err := awaitNativeTurnPublications(initialCtx, pool, env, turns)
	cancelInitial()
	if observeErr := observe("initial-30s", err); observeErr != nil {
		return errors.Join(err, observeErr)
	}
	if input.Performance.Kind != "dependencies" || !errors.Is(err, context.DeadlineExceeded) || collectionCtx.Err() != nil {
		return err
	}
	err = awaitNativeTurnPublications(collectionCtx, pool, env, turns)
	return errors.Join(err, observe("final-collection", err))
}

func retainNativePerformance(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, result []byte, input nativeExecutionConfig) error {
	var receipt struct {
		ComputerID, PeerSessionID string
		NativeSessions            map[string]string
		Turns                     []struct {
			ID, SessionID string
			Sequence      int64
			Result        struct {
				Workload struct {
					Kind                     string
					Sequence                 int64
					OperationMS, InventoryMS *float64
				} `json:"workload"`
			} `json:"result"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil {
		return err
	}
	for _, turn := range receipt.Turns {
		work := turn.Result.Workload
		if work.Kind != input.Performance.Kind || work.Sequence != turn.Sequence || work.OperationMS == nil || work.InventoryMS == nil || *work.OperationMS < 0 || *work.InventoryMS < 0 {
			return fmt.Errorf("native workload receipt missing or inconsistent for %s", turn.ID)
		}
	}
	queries := map[string]string{
		"performance-turn-lifecycle.json": `WITH native AS (
 SELECT t.*,lag(t.terminal_at) OVER(PARTITION BY t.session_id ORDER BY t.seq) AS predecessor_terminal_at
 FROM turns t JOIN sessions se ON(se.environment_id,se.id)=(t.environment_id,t.session_id)
 JOIN agents a ON(a.environment_id,a.id)=(se.environment_id,se.agent_id)
 WHERE t.environment_id=$1 AND se.computer_id=$2 AND a.key IN ('codex','claude'))
 SELECT COALESCE(jsonb_agg(jsonb_build_object('id',id,'sessionId',session_id,'sequence',seq,'createdAt',created_at,
 'startedAt',started_at,'processingClosedAt',processing_closed_at,'resultRecordedAt',result_recorded_at,'terminalAt',terminal_at,
 'firstOutputAcceptedAt',(SELECT min(e.created_at) FROM session_events e WHERE e.environment_id=native.environment_id AND e.turn_id=native.id AND e.kind='turn.output'),
 'predecessorTerminalAt',predecessor_terminal_at,'serverAdmissionToDispatchMS',extract(epoch FROM(started_at-created_at))*1000,
 'serverPredecessorSettlementToDispatchMS',extract(epoch FROM(started_at-predecessor_terminal_at))*1000) ORDER BY session_id,seq),'[]'::jsonb) FROM native`,

		"performance-turn-publications.json": `SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'turnId',t.id,'sessionId',t.session_id,'sequence',t.seq,'observedAt',statement_timestamp(),
 'resultRecordedAt',t.result_recorded_at,'terminalAt',t.terminal_at,
 'requestedSaveId',requested.id,'coveringSaveId',covering.id,'coveringStatus',covering.status,
 'publishedAt',covering.published_at,
 'resultToPublicationMS',extract(epoch FROM(covering.published_at-t.result_recorded_at))*1000)
 ORDER BY t.session_id,t.seq),'[]'::jsonb)
 FROM turns t JOIN sessions se ON(se.environment_id,se.id)=(t.environment_id,t.session_id)
 JOIN agents a ON(a.environment_id,a.id)=(se.environment_id,se.agent_id)
 LEFT JOIN computer_saves requested ON(requested.environment_id,requested.turn_id)=(t.environment_id,t.id)
 LEFT JOIN computer_saves covering ON(covering.environment_id,covering.id)=(requested.environment_id,COALESCE(requested.covered_by_save_id,requested.id))
 WHERE t.environment_id=$1 AND t.computer_id=$2 AND a.key IN ('codex','claude')`,

		"performance-saves.json": `SELECT COALESCE(jsonb_agg(jsonb_build_object('id',s.id,'turnId',s.turn_id,'sequence',s.seq,'status',s.status,
 'requestedAt',s.requested_at,'capturedAt',s.captured_at,'publishedAt',s.published_at,
 'leaseEpoch',s.computer_lease_epoch,'coverageMaxSequence',s.coverage_max_seq,'coveredBySaveId',s.covered_by_save_id,
 'retained',s.root_id IS NOT NULL,'payloadRetiredAt',s.payload_retired_at,
 'checkpoint',EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=s.environment_id AND cp.disk_save_id=s.id)) ORDER BY s.seq),'[]'::jsonb)
 FROM computer_saves s WHERE s.environment_id=$1 AND s.computer_id=$2`,
		"performance-peer-io.json": `SELECT COALESCE(jsonb_agg(jsonb_build_object('sequence',e.seq,'turnId',e.turn_id,'acceptedAt',e.created_at,
 'progress',(convert_from(e.data,'UTF8')::jsonb->0->>'text')::jsonb) ORDER BY e.seq),'[]'::jsonb)
 FROM session_events e JOIN sessions s ON(s.environment_id,s.id)=(e.environment_id,e.session_id)
 JOIN agents a ON(a.environment_id,a.id)=(s.environment_id,s.agent_id)
 WHERE e.environment_id=$1 AND s.computer_id=$2 AND a.key='peer' AND e.kind='turn.output'`,
	}
	for name, query := range queries {
		var value json.RawMessage
		if err := pool.QueryRow(ctx, query, env, receipt.ComputerID).Scan(&value); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(input.Evidence, name), value, 0600); err != nil {
			return err
		}
	}

	if err := retainNativeModelDiagnostics(ctx, pool, env, receipt.ComputerID, input.Evidence); err != nil {
		return err
	}

	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		timings, raw, err := readNativeTurnTimings(wait, pool, env, receipt.NativeSessions)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(input.Evidence, "performance-runtime-diagnostics.json"), raw, 0600); err != nil {
			return err
		}
		if len(timings) == len(receipt.Turns) {
			for _, turn := range receipt.Turns {
				timing, ok := timings[turn.ID]
				if !ok || timing.SessionID != turn.SessionID || timing.Sequence != turn.Sequence || timing.Outcome != "completed" ||
					timing.HandlerReturnToSettlementMS == nil || timing.HandlerReturnToFinalizationMS == nil || timing.FinalizationToSettlementMS == nil ||
					*timing.HandlerReturnToSettlementMS < 0 || *timing.HandlerReturnToFinalizationMS < 0 || *timing.FinalizationToSettlementMS < 0 ||
					math.Abs(*timing.HandlerReturnToSettlementMS-*timing.HandlerReturnToFinalizationMS-*timing.FinalizationToSettlementMS) > .001 {
					return fmt.Errorf("invalid runtime timing for %s", turn.ID)
				}
			}
			data, err := json.MarshalIndent(timings, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(input.Evidence, "performance-turn-timings.json"), data, 0600)
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("runtime timing records=%d expected=%d: %w", len(timings), len(receipt.Turns), wait.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func readNativeTurnTimings(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, sessions map[string]string) (map[string]nativeTurnTiming, []byte, error) {
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('sessionId',session_id,'epoch',process_epoch,'kind',kind,
 'sequence',sequence,'offset',byte_offset,'throughOffset',through_byte_offset,'data',encode(data,'base64')) ORDER BY session_id,process_epoch,sequence),'[]'::jsonb)
 FROM diagnostic_chunks WHERE environment_id=$1 AND session_id IN ($2,$3) AND stream='stderr'`, env, sessions["codex"], sessions["claude"]).Scan(&raw)
	if err != nil {
		return nil, nil, err
	}
	var chunks []struct {
		SessionID, Kind                        string
		Epoch, Sequence, Offset, ThroughOffset int64
		Data                                   []byte
	}
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return nil, raw, err
	}
	streams := map[string][]byte{}
	for _, chunk := range chunks {
		if chunk.Epoch != 1 || chunk.Kind == "gap" || chunk.Offset != int64(len(streams[chunk.SessionID])) {
			return nil, raw, errors.New("runtime diagnostic custody has a gap or changed process")
		}
		streams[chunk.SessionID] = append(streams[chunk.SessionID], chunk.Data...)
	}
	result := map[string]nativeTurnTiming{}
	for _, stream := range streams {
		for _, line := range strings.Split(string(stream), "\n") {
			start := strings.Index(line, `{"source":"helmr.runtime","event":"turn.settled",`)
			if start < 0 {
				continue
			}
			var record nativeTurnTiming
			if json.Unmarshal([]byte(line[start:]), &record) != nil {
				continue // The final diagnostic chunk may still be arriving.
			}
			if _, duplicate := result[record.TurnID]; duplicate {
				return nil, raw, errors.New("runtime timing was emitted twice for one Turn")
			}
			result[record.TurnID] = record
		}
	}
	return result, raw, nil
}
