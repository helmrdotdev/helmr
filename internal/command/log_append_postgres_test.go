package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestCommandLogProducerFenceReplayAndCompletion(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	bound := commandtest.Bound(t, f, work.LeaseID, "running")
	commandID, instanceID := bound.ID, bound.InstanceID
	producer := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	request := LogChunk{
		OrgID: f.OrgID, CommandID: commandID, InstanceID: instanceID,
		WriterGeneration: bound.WriterGeneration, Stream: LogStdout,
		ObservedSeq: 0, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte{0, 255, 128},
	}
	call := func(request LogChunk, want error) {
		t.Helper()
		if err := AppendLog(t.Context(), f.Pool, producer, request); !errors.Is(err, want) {
			t.Fatalf("append error=%v, want %v", err, want)
		}
	}
	call(request, nil)
	call(request, nil)
	for _, test := range []struct {
		name   string
		mutate func(*LogChunk)
	}{
		{"organization", func(r *LogChunk) { r.OrgID = uuid.NewV7() }},
		{"command", func(r *LogChunk) { r.CommandID = uuid.NewV7() }},
		{"instance", func(r *LogChunk) { r.InstanceID = uuid.NewV7() }},
		{"generation", func(r *LogChunk) { r.WriterGeneration++ }},
		{"changed content", func(r *LogChunk) { r.Content = []byte("different") }},
		{"changed timestamp", func(r *LogChunk) { r.ObservedAt = r.ObservedAt.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := request
			test.mutate(&mutated)
			call(mutated, ErrChanged)
		})
	}
	worker := producer
	for _, test := range []struct {
		name   string
		mutate func(*workergroup.HostPrincipal)
		want   error
	}{
		{"host", func(w *workergroup.HostPrincipal) { w.HostID = uuid.NewV7() }, ErrChanged},
		{"epoch", func(w *workergroup.HostPrincipal) { w.Epoch++ }, ErrChanged},
		{"group", func(w *workergroup.HostPrincipal) { w.GroupID = uuid.NewV7() }, ErrChanged},
		{"worker claim", func(w *workergroup.HostPrincipal) { w.HostClaimVersion++ }, workergroup.ErrStaleClaims},
		{"group claim", func(w *workergroup.HostPrincipal) { w.GroupClaimVersion++ }, workergroup.ErrStaleClaims},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.mutate(&producer)
			defer func() { producer = worker }()
			unaccepted := request
			unaccepted.ObservedSeq++
			call(unaccepted, test.want)
		})
	}
	var count int
	var bytes []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),min(encode(content,'hex')) FROM telemetry_outbox WHERE command_id=$1`, commandID).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != 1 || string(bytes) != "00ff80" {
		t.Fatalf("accepted rows=%d content=%q", count, bytes)
	}
	large := request
	large.ObservedSeq = uint64(1<<63 - 1)
	large.Content = make([]byte, telemetry.MaxRunLogContentBytes)
	call(large, nil)
	for name, invalid := range map[string]func(*LogChunk){
		"generation":  func(r *LogChunk) { r.WriterGeneration = 0 },
		"stream":      func(r *LogChunk) { r.Stream = "structured" },
		"sequence":    func(r *LogChunk) { r.ObservedSeq = 1 << 63 },
		"unobserved":  func(r *LogChunk) { r.ObservedAt = time.Time{} },
		"before 1970": func(r *LogChunk) { r.ObservedAt = time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC) },
		"after 2299":  func(r *LogChunk) { r.ObservedAt = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			mutated := request
			invalid(&mutated)
			call(mutated, ErrInvalidLog)
		})
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, instanceID)
	call(request, ErrChanged)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, instanceID)
	// Cancellation still permits the producer to flush before terminalization.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, commandID)
	request.ObservedSeq++
	call(request, nil)
	// Completion closes producer admission even before whole-instance cleanup.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='computer_command_cancelled' WHERE id=$1`, commandID)
	request.ObservedSeq++
	call(request, ErrChanged)
}
