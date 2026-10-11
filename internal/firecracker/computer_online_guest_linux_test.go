//go:build linux && computerproof

package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

// This extends the storage fixture with real guest buffered writes and a peer
// process. It is not a Session/Turn or native-harness acceptance test.
func qualifyOnlineGuestWrites(t *testing.T, machine *guestMachine, phase string, publisher *kvmPublication) disk.VersionRoot {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	computerID := machine.topology.Computer.ComputerID
	instanceID, credential := uuid.NewV7().String(), uuid.NewV7().String()
	rpc := func(kind wire.StreamType, request, response proto.Message) {
		t.Helper()
		stream, err := onlineGuestStream(ctx, machine, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := frameio.WriteProtoFrame(stream, request); err != nil {
			t.Fatal(err)
		}
		if err := frameio.ReadProtoFrameBounded(stream, 1<<20, response); err != nil {
			t.Fatal(err)
		}
	}
	var prepared computerv0.PrepareComputerRuntimeResponse
	rpc(wire.StreamTypeComputerRuntimePrepare, &computerv0.PrepareComputerRuntimeRequest{
		ComputerId: computerID, ComputerInstanceId: instanceID, WriterGeneration: 1,
		MountPath:          "/workspace",
		MountedImageConfig: &computerv0.RuntimeImageConfig{User: "0:0", WorkingDir: "/workspace", Env: []string{"PATH=/bin:/usr/bin"}},
	}, &prepared)
	if prepared.Status != "prepared" {
		t.Fatalf("prepare: %v", &prepared)
	}
	var mounted computerv0.MaterializeComputerResponse
	rpc(wire.StreamTypeComputerMaterialize, &computerv0.MaterializeComputerRequest{
		Envelope:  &computerv0.ComputerOperationEnvelope{ComputerId: computerID, ComputerInstanceId: instanceID, WriterGeneration: 1, ChannelCredential: credential},
		MountPath: "/workspace",
		Target:    &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: machine.topology.Computer.VersionID},
	}, &mounted)
	if mounted.Status != "running" {
		t.Fatalf("materialize: %v", &mounted)
	}
	run := func(script string, output func([]byte)) error {
		operation := uuid.NewV7().String()
		body, err := json.Marshal(map[string]any{"command": []string{"/bin/sh", "-ceu", script}, "cwd": "/workspace", "timeout_ms": 120000})
		if err != nil {
			return err
		}
		stream, err := onlineGuestStream(ctx, machine, wire.StreamTypeComputerBasicExec)
		if err != nil {
			return err
		}
		defer stream.Close()
		err = frameio.WriteProtoFrame(stream, &computerv0.ComputerBasicExecRequest{
			Envelope: &computerv0.ComputerCommandAuthority{OperationId: operation, ComputerId: computerID, ComputerInstanceId: instanceID, ChannelCredential: credential, WriterGeneration: 1, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(), RequestFingerprint: operation}, RequestJson: string(body),
		})
		if err != nil {
			return err
		}
		for {
			var event computerv0.ComputerBasicExecEvent
			if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &event); err != nil {
				return err
			}
			if chunk := event.GetOutput(); chunk != nil && output != nil {
				output(chunk.Content)
			}
			if result := event.GetResult(); result != nil {
				if result.Outcome != "exited" || result.ExitCode != 0 {
					return fmt.Errorf("command failed: %v", result)
				}
				return nil
			}
		}
	}
	if phase == "cold" {
		if err := run("test \"$(cat /workspace/completed)\" = initial", nil); err != nil {
			t.Fatalf("cold guest buffered write: %v", err)
		}
	}
	if err := run("rm -f /workspace/stop-peer; printf '%s' "+phase+" > /workspace/completed", nil); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan struct{}, 256)
	peerDone := make(chan error, 1)
	go func() {
		peerDone <- run("i=0; while test ! -e /workspace/stop-peer; do printf '%s' $i > /workspace/peer; printf '.\\n'; i=$((i+1)); sleep 0.02; done", func([]byte) {
			select {
			case ticks <- struct{}{}:
			default:
			}
		})
	}()
	// Join the host reader and require the guest terminal result on success.
	// Transport failures rely on the enclosing physical VM teardown.
	defer func() {
		if err := run(": > /workspace/stop-peer", nil); err != nil {
			t.Error(err)
			cancel()
		}
		if err := <-peerDone; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ticks:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cutStarted := time.Now()
	cut, err := machine.CaptureComputer(ctx)
	cutElapsed := time.Since(cutStarted)
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Capture.Release()
	// Foreground work must progress while the immutable cut remains retained.
	for len(ticks) > 0 {
		<-ticks
	}
	select {
	case <-ticks:
	case <-ctx.Done():
		t.Fatal("peer made no progress with retained cut")
	}
	publishStarted := time.Now()
	if err := cut.Capture.Publish(ctx, publisher); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: buffered guest write captured with peer progress; flush_and_cut=%s publish=%s", phase, cutElapsed, time.Since(publishStarted))
	return cut.Capture.Root()
}

// Cancellation closes the transport; Close joins the close callback before the
// caller can tear down the VM. The guest separately owns command completion.
func onlineGuestStream(ctx context.Context, machine *guestMachine, kind wire.StreamType) (io.ReadWriteCloser, error) {
	stream, err := machine.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); _ = stream.Close() })
	owned := &onlineProofStream{ReadWriteCloser: stream, stop: stop, joined: joined}
	if err := wire.WriteStreamFrameHeader(owned, wire.StreamHeader{Type: kind}, 0); err != nil {
		owned.Close()
		return nil, err
	}
	return owned, nil
}

type onlineProofStream struct {
	io.ReadWriteCloser
	stop   func() bool
	joined chan struct{}
}

func (s *onlineProofStream) Close() error {
	err := s.ReadWriteCloser.Close()
	if !s.stop() {
		<-s.joined
	}
	return err
}
