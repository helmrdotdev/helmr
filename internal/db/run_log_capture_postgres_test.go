package db_test

import (
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"testing"
	"uuid"
)

func TestAppendRunLogChunkDuringComputerCapture(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	q := db.New(f.Pool)
	for _, member := range manifest.RecoveryPoint.Runs {
		params := db.AppendRunLogChunkParams{Kind: "log.stdout", Payload: []byte(`{"stream":"stdout"}`), LeaseFenceFingerprint: "capture-log-receipt", RunLeaseID: pgvalue.UUID(uuid.MustParse(member.RunLeaseID)), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, Stream: "stdout", ObservedSeq: 1, Content: []byte("before pause")}
		first, err := q.AppendRunLogChunk(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := q.AppendRunLogChunk(t.Context(), params)
		if err != nil || !first.ReplayMatches || !replay.ReplayMatches || replay.Seq != first.Seq {
			t.Fatalf("capture log replay=%+v err=%v", replay, err)
		}
		var chunks, events int
		if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE stream_kind='run_log'),count(*) FILTER(WHERE stream_kind='event' AND kind='log.stdout') FROM telemetry_outbox WHERE run_lease_id=$1`, params.RunLeaseID).Scan(&chunks, &events); err != nil || chunks != 1 || events != 1 {
			t.Fatalf("capture log side effects chunks=%d events=%d err=%v", chunks, events, err)
		}
	}
}
