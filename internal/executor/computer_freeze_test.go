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
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type computerFreezeSession struct {
	stream net.Conn
	closed bool
}

func (s *computerFreezeSession) OpenStream(context.Context) (vm.Stream, error) { return s.stream, nil }
func (s *computerFreezeSession) Stream() vm.Stream                             { return s.stream }
func (s *computerFreezeSession) Wait(context.Context) error                    { return nil }
func (s *computerFreezeSession) Close(context.Context) error {
	s.closed = true
	return s.stream.Close()
}

func freezeTarget(count int) workerapi.RuntimeReconcileTarget {
	target := workerapi.RuntimeReconcileTarget{ID: "instance", WorkerEpoch: 2, DesiredVersion: 5, Action: workerapi.RuntimeReconcileCapture, Source: workerapi.RuntimeSource{ComputerID: "computer", ComputerSpecID: "spec", WriterGeneration: 3}, Capture: &workerapi.RuntimeCapture{CheckpointID: "checkpoint", MembershipRevision: 7, Runs: []workerapi.RuntimeCaptureRun{}}}
	if count > 0 {
		target.Capture.ProgramDeploymentID = "program"
	}
	for i := range count {
		suffix := string(rune('a' + i))
		target.Capture.Runs = append(target.Capture.Runs, workerapi.RuntimeCaptureRun{RunID: "run-" + suffix, AttemptNumber: 2, RunWaitID: "wait-" + suffix, RunLeaseID: "lease-" + suffix})
	}
	if count > 0 {
		cursor := int64(9)
		target.Capture.Runs[0].ActorSpeculativeInputSequence = &cursor
	}
	return target
}

func TestComputerFreezeVerifiesWholeGuestProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		count  int
		change func(*computerv0.FreezeComputerResponse)
	}{
		{name: "idle", count: 0}, {name: "shared", count: 2},
		{"reordered", 2, func(r *computerv0.FreezeComputerResponse) {
			r.Identity.Runs[0], r.Identity.Runs[1] = r.Identity.Runs[1], r.Identity.Runs[0]
		}},
		{"computer", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.ComputerId = "other" }},
		{"instance", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.SourceComputerInstanceId = "other" }},
		{"writer", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.WriterGeneration++ }},
		{"checkpoint", 0, func(r *computerv0.FreezeComputerResponse) { r.Identity.CheckpointId = "other" }},
		{"request version", 2, func(r *computerv0.FreezeComputerResponse) { r.DesiredVersion++ }},
		{"membership version", 2, func(r *computerv0.FreezeComputerResponse) { r.MembershipRevision++ }},
		{"missing member", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs = r.Identity.Runs[:1] }},
		{"duplicate member", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs[1] = r.Identity.Runs[0] }},
		{"wrong attempt", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs[0].AttemptNumber++ }},
		{"wrong wait", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs[0].RunWaitId = "other" }},
		{"wrong lease", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs[0].RunLeaseId = "other" }},
		{"empty correlation", 2, func(r *computerv0.FreezeComputerResponse) { r.Identity.Runs[0].CorrelationId = " " }},
		{"missing identity", 0, func(r *computerv0.FreezeComputerResponse) { r.Identity = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			target := freezeTarget(test.count)
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
				if header.Type != wire.StreamTypeComputerFreeze || header.ComputerID != target.Source.ComputerID || header.CheckpointID != target.Capture.CheckpointID || header.RunID != "" || size != 0 {
					done <- net.ErrClosed
					return
				}
				var request computerv0.FreezeComputerRequest
				if err = frameio.ReadProtoFrame(server, &request); err != nil {
					done <- err
					return
				}
				if request.ComputerInstanceId != target.ID || request.WriterGeneration != 3 || request.DesiredVersion != 5 || request.MembershipRevision != 7 || len(request.Runs) != test.count {
					done <- net.ErrClosed
					return
				}
				response := &computerv0.FreezeComputerResponse{DesiredVersion: request.DesiredVersion, MembershipRevision: request.MembershipRevision, Identity: &computerv0.ComputerRestoreIdentity{ComputerId: request.ComputerId, SourceComputerInstanceId: request.ComputerInstanceId, WriterGeneration: request.WriterGeneration, CheckpointId: request.CheckpointId}}
				for _, member := range request.Runs {
					response.Identity.Runs = append(response.Identity.Runs, &computerv0.CapturedRun{RunId: member.RunId, AttemptNumber: member.AttemptNumber, RunWaitId: member.RunWaitId, RunLeaseId: member.RunLeaseId, CorrelationId: "correlation-" + member.RunId})
				}
				if test.change != nil {
					test.change(response)
				}
				done <- frameio.WriteProtoFrame(server, response)
			}()
			session := &computerFreezeSession{stream: client}
			point, err := guestControl{machine: session}.freeze(ctx, target)
			wantError := test.change != nil && test.name != "reordered"
			if (err != nil) != wantError {
				t.Fatalf("freeze: %v", err)
			}
			if !wantError {
				if point.ComputerID != target.Source.ComputerID || point.ComputerInstanceID != target.ID || point.ComputerSpecID != "spec" || point.ProgramDeploymentID != target.Capture.ProgramDeploymentID || point.Runs == nil || len(point.Runs) != test.count {
					t.Fatalf("point=%+v", point)
				}
				for i, member := range point.Runs {
					if member.RunID != target.Capture.Runs[i].RunID || member.CorrelationID != "correlation-"+member.RunID {
						t.Fatalf("member=%+v", member)
					}
				}
				if test.count > 0 {
					if point.Runs[0].ActorSpeculativeInputSequence == nil || *point.Runs[0].ActorSpeculativeInputSequence != 9 || point.Runs[1].ActorSpeculativeInputSequence != nil {
						t.Fatal("cursor changed")
					}
					*point.Runs[0].ActorSpeculativeInputSequence = 10
					if *target.Capture.Runs[0].ActorSpeculativeInputSequence != 9 {
						t.Fatal("source cursor aliased")
					}
				}
			}
			if session.closed {
				t.Fatal("freeze RPC closed the physical session")
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestComputerFreezeRejectsInvalidIntentBeforeOpeningStream(t *testing.T) {
	for _, change := range []func(*workerapi.RuntimeReconcileTarget){
		func(t *workerapi.RuntimeReconcileTarget) { t.Capture = nil },
		func(t *workerapi.RuntimeReconcileTarget) { t.Action = workerapi.RuntimeReconcilePrepare },
		func(t *workerapi.RuntimeReconcileTarget) { t.Source.WriterGeneration = 0 },
		func(t *workerapi.RuntimeReconcileTarget) { t.Capture.ProgramDeploymentID = "" },
		func(t *workerapi.RuntimeReconcileTarget) { t.Capture.Runs[1] = t.Capture.Runs[0] },
		func(t *workerapi.RuntimeReconcileTarget) { t.Capture.Runs[0].AttemptNumber = -1 },
		func(t *workerapi.RuntimeReconcileTarget) { t.Capture.Runs[0].RunLeaseID = "" },
	} {
		target := freezeTarget(2)
		change(&target)
		if _, err := computerFreezeRequest(target); err == nil {
			t.Fatal("invalid intent accepted")
		}
	}
}

func TestComputerFreezeCancellationClosesBlockedStreamOnly(t *testing.T) {
	for _, readRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "request write", true: "response read"}[readRequest], func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session := &computerFreezeSession{stream: client}
			done := make(chan error, 1)
			go func() { _, err := guestControl{machine: session}.freeze(ctx, freezeTarget(0)); done <- err }()
			if _, _, err := wire.ReadStreamFrameHeader(server); err != nil {
				t.Fatal(err)
			}
			if readRequest {
				var request computerv0.FreezeComputerRequest
				if err := frameio.ReadProtoFrame(server, &request); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled freeze succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked stream did not cancel")
			}
			if session.closed {
				t.Fatal("RPC owns source exclusion")
			}
		})
	}
}
