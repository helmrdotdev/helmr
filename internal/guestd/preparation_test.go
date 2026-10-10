package guestd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/wire"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func preparationGuestFixture(t *testing.T) (*computerOperationRegistry, *computerMountEntry, *computerv0.PreparationControlRequest) {
	t.Helper()
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	credential := bytes.Repeat([]byte{11}, 32)
	entry := &computerMountEntry{computerID: "preparation", computerInstanceID: "instance", writerGeneration: 1, channelCredential: base64.RawURLEncoding.EncodeToString(credential)}
	registry := newComputerOperationRegistry()
	if err := registry.register("instance", entry); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entry.lifecycleMu.Lock()
		p := entry.preparation
		entry.lifecycleMu.Unlock()
		if p != nil {
			p.cancel()
			select {
			case <-p.done:
			case <-time.After(time.Second):
				t.Error("preparation did not join")
			}
			p.output.close(false)
		}
	})
	return registry, entry, &computerv0.PreparationControlRequest{Identity: &computerv0.PreparationIdentity{PreparationId: "preparation", InstanceId: "instance", Epoch: 1, ChannelCredential: credential}, ExpiresAtUnixNano: time.Now().Add(time.Second).UnixNano()}
}

func TestPreparationRetainsOneExecutionAndReplaysOutput(t *testing.T) {
	registry, entry, request := preparationGuestFixture(t)
	var runs atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	run := func(_ *computerMountEntry, ctx context.Context, _ *computerv0.PreparationStart, output *preparationOutput) (error, error) {
		runs.Add(1)
		close(started)
		if _, err := (preparationOutputWriter{buffer: output.stream("stdout")}).Write([]byte("prepared")); err != nil {
			return err, nil
		}
		select {
		case <-finish:
			output.close(true)
			return nil, nil
		case <-ctx.Done():
			return ctx.Err(), nil
		}
	}
	absent, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || absent.State != "absent" || runs.Load() != 0 {
		t.Fatalf("probe started code: %+v %v", absent, err)
	}
	request.Start = &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo"}
	if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
		t.Fatal(err)
	}
	<-started
	// A lost start response is reconciled by probing, never another start.
	if _, err := registry.controlPreparation(t.Context(), request, run); err == nil {
		t.Fatal("duplicate start accepted")
	}
	request.Start = nil
	for range 3 {
		if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
			t.Fatal(err)
		}
	}
	wrong := proto.Clone(request).(*computerv0.PreparationControlRequest)
	wrong.Start = &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "other"}
	if _, err := registry.controlPreparation(t.Context(), wrong, run); err == nil {
		t.Fatal("changed Program was accepted")
	}
	close(finish)
	<-entry.preparation.done
	request.Start = nil
	request.LogStream = "stdout"
	result, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || result.State != "succeeded" || result.GetLog().GetKind() != "data" || string(result.GetLog().GetData()) != "prepared" || runs.Load() != 1 {
		t.Fatalf("retained result: %+v %v runs=%d", result, err, runs.Load())
	}
	request.LogStream = ""
	request.RenewOnly = true
	renewed, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || renewed.Log != nil || renewed.State != "succeeded" {
		t.Fatalf("renewal read output: %+v %v", renewed, err)
	}
	request.RenewOnly = false
	request.LogStream = "stdout"
	again, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || !proto.Equal(again, result) {
		t.Fatalf("output replay changed: %v", err)
	}
	request.AcknowledgedThrough = result.Log.ThroughSequence
	end, err := registry.controlPreparation(t.Context(), request, run)
	if err != nil || end.GetLog().GetKind() != "end" || !end.GetLog().GetComplete() || end.State != "succeeded" {
		t.Fatalf("end cursor: %+v %v", end, err)
	}
}

func TestPreparationRenewalExpiryAndCleanupFailure(t *testing.T) {
	for _, mode := range []string{"expiry", "cleanup"} {
		t.Run(mode, func(t *testing.T) {
			registry, entry, request := preparationGuestFixture(t)
			request.ExpiresAtUnixNano = time.Now().Add(100 * time.Millisecond).UnixNano()
			request.Start = &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo"}
			started := make(chan struct{})
			run := func(_ *computerMountEntry, ctx context.Context, _ *computerv0.PreparationStart, _ *preparationOutput) (error, error) {
				close(started)
				if mode == "cleanup" {
					return nil, errors.New("descendants not excluded")
				}
				<-ctx.Done()
				return ctx.Err(), nil
			}
			if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
				t.Fatal(err)
			}
			<-started
			request.Start = nil
			if mode != "cleanup" {
				request.ExpiresAtUnixNano = time.Now().Add(250 * time.Millisecond).UnixNano()
				if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
					t.Fatal(err)
				}
				time.Sleep(150 * time.Millisecond)
				select {
				case <-entry.preparation.done:
					t.Fatal("renewal failed to retain preparation")
				default:
				}

			}
			select {
			case <-entry.preparation.done:
			case <-time.After(time.Second):
				t.Fatal("preparation did not stop")
			}
			response, err := registry.controlPreparation(t.Context(), request, run)
			if err != nil || response.State != "failed" {
				t.Fatalf("terminal state: %+v %v", response, err)
			}
			if mode == "cleanup" {
				entry.processesMu.Lock()
				blocked := entry.recoveryRequired
				entry.processesMu.Unlock()
				if !blocked || response.ErrorCode != "preparation_scope_termination_failed" {
					t.Fatal("cleanup failure permitted capture")
				}
			}
			request.Start = &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo"}
			request.ExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
			replay, err := registry.controlPreparation(t.Context(), request, run)
			if err == nil || replay != nil {
				t.Fatalf("terminal executor restarted: %+v %v", replay, err)
			}
		})
	}
}

func TestPreparationControlSecretFrameBounds(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "large valid payload", true: "over limit"}[oversized], func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			done := make(chan error, 1)
			go func() {
				defer guest.Close()
				done <- handlePreparationControl(t.Context(), guest, 0, newComputerOperationRegistry())
			}()
			if oversized {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], wire.PreparationControlFrameBytes+1)
				_, _ = host.Write(header[:])
			} else {
				request := &computerv0.PreparationControlRequest{Identity: &computerv0.PreparationIdentity{PreparationId: "missing", InstanceId: "missing", Epoch: 1, ChannelCredential: bytes.Repeat([]byte{1}, 32)}, Start: &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo", Secrets: []*computerv0.ComputerSecretDelivery{{PlacementKind: "env", PlacementTarget: "LARGE", Value: bytes.Repeat([]byte{65}, 80<<10)}}}}
				if err := frameio.WriteProtoFrame(host, request); err != nil {
					t.Fatal(err)
				}
			}
			err := <-done
			if oversized {
				if err == nil || !strings.Contains(err.Error(), "exceeds max") {
					t.Fatalf("over limit accepted: %v", err)
				}
			} else if err == nil || err.Error() != "preparation does not own mounted image" {
				t.Fatalf("valid Secret frame did not reach authorization: %v", err)
			}
		})
	}
}
