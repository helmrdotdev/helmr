package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestCommandLogProducerFenceReplayAndCompletion(t *testing.T) {
	f := commandFixture(t)
	bound := f.start(t, "log-command")
	commandID, instanceID := uuid.UUID(bound.Command.ID.Bytes), bound.Lease.InstanceID
	producer := f.host()
	request := LogChunk{
		EnvironmentID: f.Environment, CommandID: commandID, InstanceID: instanceID,
		WriterGeneration: bound.Lease.Epoch, Stream: LogStdout,
		Kind: "data", ThroughSequence: 1, ObservedSeq: 1, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte{0, 255, 128},
	}
	call := func(request LogChunk, want error) {
		t.Helper()
		if _, err := AppendLog(t.Context(), f.Pool, producer, request, diagnostic.Bounds{ChunkBytes: 192 << 10, SourceBytes: 1 << 20, SourceRecords: 16, EnvironmentBytes: 2 << 20, EnvironmentRecords: 32, QueueBytes: 4 << 20, QueueRecords: 64}); !errors.Is(err, want) {
			t.Fatalf("append error=%v, want %v", err, want)
		}
	}
	call(request, nil)
	call(request, nil)
	for _, test := range []struct {
		name   string
		mutate func(*LogChunk)
	}{
		{"organization", func(r *LogChunk) { r.EnvironmentID = uuid.NewV7() }},
		{"command", func(r *LogChunk) { r.CommandID = uuid.NewV7() }},
		{"instance", func(r *LogChunk) { r.InstanceID = uuid.NewV7() }},
		{"generation", func(r *LogChunk) { r.WriterGeneration++ }},
		{"changed content", func(r *LogChunk) { r.Content = []byte("different") }},
		{"changed timestamp", func(r *LogChunk) { r.ObservedAt = r.ObservedAt.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := request
			test.mutate(&mutated)
			want := ErrChanged
			if test.name == "changed content" || test.name == "changed timestamp" {
				want = telemetry.ErrDiagnosticConflict
			}
			call(mutated, want)
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
			unaccepted.ThroughSequence++
			call(unaccepted, test.want)
		})
	}
	var count int
	var bytes []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),min(encode(data,'hex')) FROM telemetry_outbox WHERE command_id=$1`, commandID).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != 1 || string(bytes) != "00ff80" {
		t.Fatalf("accepted rows=%d content=%q", count, bytes)
	}
	large := request
	large.ObservedSeq = 2
	large.ThroughSequence = 2
	const largeCommandChunkBytes = 192 << 10
	large.Content = make([]byte, largeCommandChunkBytes)
	call(large, nil)
	request = large
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
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`, instanceID)
	call(request, ErrChanged)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '5 minutes' WHERE computer_instance_id=$1`, instanceID)
	// Cancellation still permits the producer to flush before terminalization.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, commandID)
	request.ObservedSeq++
	request.ThroughSequence++
	call(request, nil)
	// Completion closes producer admission even before whole-instance cleanup.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='computer_command_cancelled' WHERE id=$1`, commandID)
	request.ObservedSeq++
	request.ThroughSequence++
	call(request, ErrChanged)
}
