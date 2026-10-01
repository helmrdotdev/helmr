package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type leaseDiscoveryStore struct {
	rows   []db.DiscoverWorkerRunLeaseWorkRow
	err    error
	params db.DiscoverWorkerRunLeaseWorkParams
}

func (s *leaseDiscoveryStore) DiscoverWorkerRunLeaseWork(_ context.Context, params db.DiscoverWorkerRunLeaseWorkParams) ([]db.DiscoverWorkerRunLeaseWorkRow, error) {
	s.params = params
	return s.rows, s.err
}

func TestDiscoverLeasesReturnsOnlyExactWorkTuples(t *testing.T) {
	groupID, hostID := uuid.NewV7(), uuid.NewV7()
	first, second := uuid.NewV7(), uuid.NewV7()
	store := &leaseDiscoveryStore{rows: []db.DiscoverWorkerRunLeaseWorkRow{
		{ID: pgvalue.UUID(first), LeaseSequence: 3},
		{ID: pgvalue.UUID(second), LeaseSequence: 7},
	}}
	work, err := run.DiscoverLeases(context.Background(), store, groupID, hostID, 11)
	if err != nil {
		t.Fatal(err)
	}
	if store.params.WorkerGroupID != pgvalue.UUID(groupID) || store.params.WorkerHostID != pgvalue.UUID(hostID) || store.params.WorkerEpoch != 11 || store.params.RowLimit != 64 {
		t.Fatalf("discovery params = %+v", store.params)
	}
	if len(work) != 2 || work[0] != (run.LeaseWork{LeaseID: first, LeaseSequence: 3}) || work[1] != (run.LeaseWork{LeaseID: second, LeaseSequence: 7}) {
		t.Fatalf("discovered work = %+v", work)
	}
}

func TestDiscoverLeasesReturnsAnEmptyList(t *testing.T) {
	work, err := run.DiscoverLeases(context.Background(), &leaseDiscoveryStore{}, uuid.NewV7(), uuid.NewV7(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if work == nil || len(work) != 0 {
		t.Fatalf("empty discovery = %+v", work)
	}
}

func TestDiscoverLeasesPropagatesStorageFailure(t *testing.T) {
	expected := errors.New("database unavailable")
	if _, err := run.DiscoverLeases(context.Background(), &leaseDiscoveryStore{err: expected}, uuid.NewV7(), uuid.NewV7(), 1); !errors.Is(err, expected) {
		t.Fatalf("discovery error = %v, want %v", err, expected)
	}
}

type logStore struct {
	replay        *db.GetRunLogChunkReplayRow
	replayMatches bool
	workerHostID  pgtype.UUID
	appended      *db.AppendRunLogChunkParams
}

func (s logStore) GetRunLogChunkReplay(context.Context, db.GetRunLogChunkReplayParams) (db.GetRunLogChunkReplayRow, error) {
	if s.replay == nil {
		return db.GetRunLogChunkReplayRow{}, pgx.ErrNoRows
	}
	return *s.replay, nil
}

func (s logStore) AppendRunLogChunk(_ context.Context, params db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	if s.appended != nil {
		*s.appended = params
	}
	if s.workerHostID.Valid && params.WorkerHostID != s.workerHostID {
		return db.AppendRunLogChunkRow{}, pgx.ErrNoRows
	}
	return db.AppendRunLogChunkRow{ReplayMatches: s.replayMatches}, nil
}

func logChunk() run.LogChunk {
	return run.LogChunk{
		Fence:            run.ExecutionFence{LeaseID: pgvalue.UUID(uuid.NewV7()), LeaseSequence: 4, WorkerGroupID: pgvalue.UUID(uuid.NewV7()), WorkerHostID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 2, GroupClaimVersion: 9, HostClaimVersion: 9},
		FenceFingerprint: "fence",
		Kind:             "log.stdout", Severity: "info", Stream: "stdout", ObservedSeq: 1,
		Payload: json.RawMessage(`{"bytes":5,"observed_seq":1,"stream":"stdout"}`),
		Content: []byte("alpha"),
	}
}

func TestAppendLogAppendsUnderTheLeaseFence(t *testing.T) {
	chunk := logChunk()
	var params db.AppendRunLogChunkParams
	if err := run.AppendLog(context.Background(), logStore{replayMatches: true, appended: &params}, chunk); err != nil {
		t.Fatal(err)
	}
	fence := chunk.Fence
	if params.RunLeaseID != fence.LeaseID || params.LeaseSequence != fence.LeaseSequence || params.WorkerGroupID != fence.WorkerGroupID || params.WorkerHostID != fence.WorkerHostID || params.WorkerEpoch != fence.WorkerEpoch || params.LeaseFenceFingerprint != chunk.FenceFingerprint || params.Stream != chunk.Stream || params.ObservedSeq != chunk.ObservedSeq || string(params.Content) != "alpha" {
		t.Fatalf("append params = %+v", params)
	}
}

func TestAppendLogRejectsAChangedConcurrentAppend(t *testing.T) {
	if err := run.AppendLog(context.Background(), logStore{replayMatches: false}, logChunk()); !errors.Is(err, run.ErrLogChunkDiffers) {
		t.Fatalf("changed append = %v", err)
	}
}

func TestAppendLogReplaysAfterLeaseIsNoLongerLive(t *testing.T) {
	chunk := logChunk()
	replay := db.GetRunLogChunkReplayRow{Content: []byte("alpha"), EventPayload: `{"stream":"stdout","observed_seq":1,"bytes":5}`, LeaseFenceFingerprint: chunk.FenceFingerprint}
	var appended db.AppendRunLogChunkParams
	if err := run.AppendLog(context.Background(), logStore{replay: &replay, appended: &appended}, chunk); err != nil {
		t.Fatal(err)
	}
	if appended.Stream != "" {
		t.Fatal("a recorded chunk was appended again")
	}
	for name, mutate := range map[string]func(*db.GetRunLogChunkReplayRow){
		"content":     func(r *db.GetRunLogChunkReplayRow) { r.Content = []byte("beta") },
		"payload":     func(r *db.GetRunLogChunkReplayRow) { r.EventPayload = `{"bytes":6,"observed_seq":1,"stream":"stdout"}` },
		"fingerprint": func(r *db.GetRunLogChunkReplayRow) { r.LeaseFenceFingerprint = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := replay
			mutate(&changed)
			if err := run.AppendLog(context.Background(), logStore{replay: &changed}, chunk); !errors.Is(err, run.ErrLogChunkDiffers) {
				t.Fatalf("changed replay = %v", err)
			}
		})
	}
}

func TestAppendLogRejectsAnotherWorkersFence(t *testing.T) {
	chunk := logChunk()
	var appended db.AppendRunLogChunkParams
	err := run.AppendLog(context.Background(), logStore{replayMatches: true, workerHostID: pgvalue.UUID(uuid.NewV7()), appended: &appended}, chunk)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another worker's append = %v", err)
	}
	if appended.WorkerHostID != chunk.Fence.WorkerHostID {
		t.Fatal("the append statement did not fence another worker's receipt")
	}
}

type taskCompletionReplays struct {
	fingerprint pgtype.Text
	err         error
	calls       int
	last        db.GetTaskCompletionReplayParams
}

func (r *taskCompletionReplays) GetTaskCompletionReplay(_ context.Context, params db.GetTaskCompletionReplayParams) (pgtype.Text, error) {
	r.calls++
	r.last = params
	return r.fingerprint, r.err
}

func replayedTaskCompletion() run.TaskCompletion {
	return run.TaskCompletion{
		Fence:       run.ExecutionFence{LeaseID: pgvalue.UUID(uuid.NewV7()), LeaseSequence: 7, WorkerGroupID: pgvalue.UUID(uuid.NewV7()), WorkerHostID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 2},
		OperationID: pgvalue.UUID(uuid.NewV7()), Fingerprint: "sha256:receipt", Kind: "succeeded", Output: json.RawMessage(`{}`),
	}
}

// A completion whose transaction cannot run still accepts its recorded
// receipt, read by the lease receipt alone.
func TestCompleteTaskAcceptsRecordedReceiptAfterFailure(t *testing.T) {
	completion := replayedTaskCompletion()
	replays := &taskCompletionReplays{fingerprint: pgvalue.Text(completion.Fingerprint)}
	if err := run.CompleteTask(context.Background(), nil, replays, completion); err != nil {
		t.Fatalf("recorded receipt = %v", err)
	}
	fence := completion.Fence
	if replays.calls != 1 || replays.last.RunLeaseID != fence.LeaseID || replays.last.LeaseSequence != 7 || replays.last.WorkerGroupID != fence.WorkerGroupID || replays.last.WorkerHostID != fence.WorkerHostID {
		t.Fatalf("replay lookups = %d, selector = %+v", replays.calls, replays.last)
	}
}

func TestCompleteTaskRejectsChangedRecordedReceipt(t *testing.T) {
	replays := &taskCompletionReplays{fingerprint: pgvalue.Text("sha256:committed")}
	err := run.CompleteTask(context.Background(), nil, replays, replayedTaskCompletion())
	if !errors.Is(err, run.ErrTaskCompletionReplayDiffers) || !errors.Is(err, run.ErrStale) {
		t.Fatalf("changed receipt = %v", err)
	}
}

func TestCompleteTaskKeepsFailureWithoutRecordedReceipt(t *testing.T) {
	err := run.CompleteTask(context.Background(), nil, &taskCompletionReplays{err: pgx.ErrNoRows}, replayedTaskCompletion())
	if err == nil || errors.Is(err, run.ErrStale) {
		t.Fatalf("unrecorded failure = %v", err)
	}
	lookup := errors.New("replay unavailable")
	err = run.CompleteTask(context.Background(), nil, &taskCompletionReplays{err: lookup}, replayedTaskCompletion())
	if !errors.Is(err, lookup) || errors.Is(err, run.ErrStale) {
		t.Fatalf("failed replay lookup = %v", err)
	}
}
