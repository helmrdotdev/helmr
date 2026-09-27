package executor

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type restoreVerificationSession struct{ stream net.Conn }

func (s restoreVerificationSession) OpenStream(context.Context) (vm.Stream, error) {
	return s.stream, nil
}
func (s restoreVerificationSession) Stream() vm.Stream           { return s.stream }
func (s restoreVerificationSession) Wait(context.Context) error  { return nil }
func (s restoreVerificationSession) Close(context.Context) error { return s.stream.Close() }

func TestRestoreVerificationChecksCompleteGuestResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		count  int
		change func(*computerv0.ComputerRestoreIdentity)
	}{
		{name: "idle", count: 0}, {name: "shared", count: 2},
		{"missing member", 2, func(i *computerv0.ComputerRestoreIdentity) { i.Runs = i.Runs[:1] }},
		{"source instance", 2, func(i *computerv0.ComputerRestoreIdentity) { i.SourceComputerInstanceId = "other" }},
		{"generation", 2, func(i *computerv0.ComputerRestoreIdentity) { i.WriterGeneration++ }},
		{"checkpoint", 0, func(i *computerv0.ComputerRestoreIdentity) { i.CheckpointId = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			identity := &computerv0.ComputerRestoreIdentity{ComputerId: "computer", SourceComputerInstanceId: "source", WriterGeneration: 3, CheckpointId: "checkpoint"}
			for i := range test.count {
				key := string(rune('a' + i))
				identity.Runs = append(identity.Runs, &computerv0.CapturedRun{RunId: "run-" + key, AttemptNumber: 2, RunWaitId: "wait-" + key, RunLeaseId: "lease-" + key, CorrelationId: "correlation-" + key})
			}
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				header, size, err := wire.ReadStreamFrameHeader(server)
				if err != nil {
					done <- err
					return
				}
				if header.Type != wire.StreamTypeComputerRestoreVerify || header.ComputerID != identity.ComputerId || header.CheckpointID != identity.CheckpointId || header.RunID != "" || size != 0 {
					done <- net.ErrClosed
					return
				}
				var request computerv0.VerifyComputerRestoreRequest
				if err := frameio.ReadProtoFrame(server, &request); err != nil {
					done <- err
					return
				}
				if !proto.Equal(request.Identity, identity) {
					done <- net.ErrClosed
					return
				}
				response := proto.Clone(request.Identity).(*computerv0.ComputerRestoreIdentity)
				if test.change != nil {
					test.change(response)
				}
				done <- frameio.WriteProtoFrame(server, &computerv0.VerifyComputerRestoreResponse{Identity: response})
			}()
			err := verifyRestoredComputerOnSession(ctx, restoreVerificationSession{client}, &computerv0.VerifyComputerRestoreRequest{Identity: identity})
			if (err != nil) != (test.change != nil) {
				t.Fatalf("verification: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
