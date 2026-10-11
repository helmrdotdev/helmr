package command

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"testing"
	"time"
	"uuid"
)

func TestCommandTerminalAndTailResumeUnderDiagnosticPressure(t *testing.T) {
	f := commandFixture(t)
	start := f.start(t, "bounded-output")
	id := uuid.UUID(start.Command.ID.Bytes)
	bounds := diagnostic.Bounds{ChunkBytes: 16, SourceBytes: 32, SourceRecords: 1, EnvironmentBytes: 32, EnvironmentRecords: 1, QueueBytes: 32, QueueRecords: 1}
	log := LogChunk{EnvironmentID: f.Environment, CommandID: id, InstanceID: start.Lease.InstanceID, WriterGeneration: start.Lease.Epoch, Stream: "stdout", Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now(), Content: []byte{0, 255, 128}}
	first, err := AppendLog(t.Context(), f.Pool, f.host(), log, bounds)
	if err != nil {
		t.Fatal(err)
	}
	code := int32(0)
	report := CompletionReport{EnvironmentID: f.Environment, CommandID: id, InstanceID: start.Lease.InstanceID, WriterGeneration: start.Lease.Epoch, Outcome: "exited", ExitCode: &code, Stdout: OutputBoundary{ThroughSequence: 2, Complete: true}, Stderr: OutputBoundary{ThroughSequence: 1, Complete: true}}
	if err := Complete(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal("full telemetry queue blocked terminal", err)
	}
	if err := Reconcile(t.Context(), f.Pool, f.host(), report); !errors.Is(err, ErrChanged) {
		t.Fatal("unsettled output reconciled", err)
	}
	claim, err := Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || claim.Start == nil || !claim.Start.TailOnly || claim.Release != nil || len(claim.Start.Secrets) != 0 {
		t.Fatalf("tail resume: %+v %v", claim, err)
	}
	if err := CheckStart(t.Context(), f.Pool, f.host(), *claim.Start); err != nil {
		t.Fatal(err)
	}
	end := log
	end.Kind = "end"
	end.ObservedSeq = 2
	end.ThroughSequence = 2
	end.Content = nil
	end.Complete = true
	if _, err := AppendLog(t.Context(), f.Pool, f.host(), end, bounds); !errors.Is(err, telemetry.ErrDiagnosticCapacity) {
		t.Fatal("expected queue pressure", err)
	}
	export := func() telemetry.DiagnosticExportClaim {
		t.Helper()
		batch, err := telemetry.ClaimDiagnostics(t.Context(), f.Pool, "computer_command", diagnostic.ExportBounds{Records: 4, Bytes: 64, ClaimFor: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := telemetry.RetireDiagnostics(t.Context(), f.Pool, batch.Token, batch.IDs); err != nil {
			t.Fatal(err)
		}
		return batch
	}
	batch := export()
	if len(batch.Records) != 1 || !bytes.Equal(batch.Records[0].Record.Data, log.Content) {
		t.Fatal("accepted binary output lost", batch)
	}
	retry, err := AppendLog(t.Context(), f.Pool, f.host(), log, bounds)
	if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) || !retry.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("lost receipt identity", retry, err)
	}
	if _, err := AppendLog(t.Context(), f.Pool, f.host(), end, bounds); err != nil {
		t.Fatal(err)
	}
	export()
	end.Stream = "stderr"
	end.ObservedSeq = 1
	end.ThroughSequence = 1
	if _, err := AppendLog(t.Context(), f.Pool, f.host(), end, bounds); err != nil {
		t.Fatal(err)
	}
	claim, err = Claim(t.Context(), f.Pool, f.host(), f.claim())
	if err != nil || claim.Release == nil || claim.Start != nil {
		t.Fatalf("settled release %+v %v", claim, err)
	}
	if err := Reconcile(t.Context(), f.Pool, f.host(), report); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendLog(t.Context(), f.Pool, f.host(), end, bounds); !errors.Is(err, ErrChanged) {
		t.Fatal("reconciled producer accepted output", err)
	}
}
