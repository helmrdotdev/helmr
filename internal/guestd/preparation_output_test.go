package guestd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func TestPreparationSaturationKeepsExecutionAndStreamsIndependent(t *testing.T) {
	registry, entry, request := preparationGuestFixture(t)
	request.Start = &computerv0.PreparationStart{ComputerDefinitionId: "repo", LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 3, BufferBytes: 3, BufferRecords: 1}}
	written := make(chan struct{})
	finish := make(chan struct{})
	run := func(_ *computerMountEntry, ctx context.Context, _ *computerv0.PreparationStart, output *preparationOutput) (error, error) {
		stdout := preparationOutputWriter{output.stream("stdout")}
		_, _ = stdout.Write([]byte{0, 255, 128})
		// This output exceeds the full source budget; it must still drain completely.
		data := bytes.Repeat([]byte("x"), 30000)
		if n, err := stdout.Write(data); n != len(data) || err != nil {
			t.Errorf("drain=%d %v", n, err)
		}
		_, _ = (preparationOutputWriter{output.stream("stderr")}).Write([]byte("err"))
		close(written)
		select {
		case <-finish:
		case <-ctx.Done():
			return ctx.Err(), nil
		}
		output.pipeClosed("stdout", nil)
		output.pipeClosed("stderr", errors.New("pipe lost"))
		return nil, nil
	}
	if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
		t.Fatal(err)
	}
	<-written
	request.Start = nil
	request.LogStream = "stdout"
	first, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || !bytes.Equal(first.GetLog().GetData(), []byte{0, 255, 128}) {
		t.Fatalf("head: %v %v", first, err)
	}
	for range 3 {
		again, err := registry.controlPreparation(t.Context(), request, run)
		if err != nil || !proto.Equal(first, again) {
			t.Fatalf("uncertain replay changed: %v %v", again, err)
		}
	}
	wrong := proto.Clone(request).(*computerv0.PreparationControlRequest)
	wrong.AcknowledgedThrough = 2
	if _, err := registry.controlPreparation(t.Context(), wrong, run); err == nil {
		t.Fatal("unaccepted range skipped")
	}
	wrong = proto.Clone(request).(*computerv0.PreparationControlRequest)
	wrong.Identity.Epoch++
	if _, err := registry.controlPreparation(t.Context(), wrong, run); err == nil {
		t.Fatal("foreign epoch accepted")
	}
	request.LogStream = "stderr"
	other, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || string(other.GetLog().GetData()) != "err" {
		t.Fatalf("independent stderr: %v %v", other, err)
	}
	request.LogStream = ""
	request.RenewOnly = true
	request.ExpiresAtUnixNano = time.Now().Add(time.Minute).UnixNano()
	if renewed, err := registry.controlPreparation(t.Context(), request, run); err != nil || renewed.Log != nil || renewed.State != "running" {
		t.Fatalf("renewal: %v %v", renewed, err)
	}
	close(finish)
	<-entry.preparation.done
	request.RenewOnly = false
	state, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || state.State != "succeeded" || state.Log != nil {
		t.Fatalf("execution blocked by unaccepted output: %v %v", state, err)
	}
	request.LogStream = "stdout"
	request.AcknowledgedThrough = 1
	gap, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || gap.GetLog().GetKind() != "gap" || gap.Log.Sequence != 2 || gap.Log.ThroughSequence != 10001 || gap.Log.DroppedBytes != 30000 {
		t.Fatalf("loss accounting: %v %v", gap, err)
	}
	// Replaying the data receipt cannot mutate the exposed gap.
	again, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || !proto.Equal(gap, again) {
		t.Fatalf("receipt replay: %v %v", again, err)
	}
	request.AcknowledgedThrough = gap.Log.ThroughSequence
	end, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || end.GetLog().GetKind() != "end" || !end.Log.Complete || end.Log.Sequence != 10002 {
		t.Fatalf("exact EOF: %v %v", end, err)
	}
	request.LogStream = "stderr"
	request.AcknowledgedThrough = 1
	end, err = registry.controlPreparation(t.Context(), request, run)
	if err != nil || end.GetLog().GetKind() != "end" || end.Log.Complete {
		t.Fatalf("unknown pipe boundary: %v %v", end, err)
	}
}

func TestPreparationActualPipeEOFSurvivesNonzeroExit(t *testing.T) {
	output, err := newPreparationOutput(&computerv0.PreparationLogLimits{ChunkBytes: 32, BufferBytes: 64, BufferRecords: 4})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf stdout; printf stderr >&2; exit 7")
	cmd.Stdout = preparationOutputWriter{output.stream("stdout")}
	cmd.Stderr = preparationOutputWriter{output.stream("stderr")}
	runErr, cleanupErr := runScopedCommand(cmd, &commandTestScope{}, output.pipeClosed)
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 || cleanupErr != nil {
		t.Fatalf("run=%v cleanup=%v", runErr, cleanupErr)
	}
	output.close(false) // The registry's fallback must not overwrite actual EOF.
	for _, stream := range []string{"stdout", "stderr"} {
		buffer := output.stream(stream)
		data, ok := buffer.peek()
		if !ok || string(data.Data) != stream {
			t.Fatalf("%s bytes: %v", stream, data)
		}
		if err := buffer.acknowledge(data.Through); err != nil {
			t.Fatal(err)
		}
		end, ok := buffer.peek()
		if !ok || end.Kind != diagnosticEnd || !end.Complete {
			t.Fatalf("%s EOF: %v", stream, end)
		}
	}
}
