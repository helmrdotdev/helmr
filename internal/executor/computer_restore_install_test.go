package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

type restoreActivationHarness struct {
	mountedMachine
	mu                              sync.Mutex
	calls                           []string
	failAck, alterAck, alterInstall bool
	ack                             workerapi.ComputerRestoreAckRequest
	installation                    *computerv0.ComputerRestoreInstallation
}

func (h *restoreActivationHarness) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() {
		defer guest.Close()
		header, _, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			return
		}
		var q computerv0.ComputerRestoreInstallation
		if frameio.ReadProtoFrame(guest, &q) != nil {
			return
		}
		h.mu.Lock()
		h.calls = append(h.calls, string(header.Type))
		h.installation = &q
		alter := h.alterInstall && header.Type == wire.StreamTypeComputerRestoreInstall
		h.mu.Unlock()
		if alter {
			q.DesiredVersion++
		}
		_ = frameio.WriteProtoFrame(guest, &computerv0.ComputerRestoreInstallationResponse{Installation: &q})
	}()
	return host, nil
}
func (h *restoreActivationHarness) AcknowledgeComputerRestore(_ context.Context, q workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "ack")
	h.ack = q
	if h.failAck {
		return workerapi.ComputerRestoreAckResponse{}, errors.New("ack failed")
	}
	result := workerapi.ComputerRestoreAckResponse{ComputerInstanceID: q.ComputerInstanceID, CheckpointID: q.CheckpointID, DesiredVersion: q.DesiredVersion, WriterGeneration: q.WriterGeneration}
	if h.alterAck {
		result.DesiredVersion++
	}
	return result, nil
}
func TestRestoreActivationOrdersGuestInstallationBeforeDurableAck(t *testing.T) {
	for _, name := range []string{"success", "ack failure", "altered ack", "altered install"} {
		t.Run(name, func(t *testing.T) {
			h := &restoreActivationHarness{failAck: name == "ack failure", alterAck: name == "altered ack", alterInstall: name == "altered install"}
			q := &computerv0.ComputerRestoreInstallation{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 4, ChannelToken: "channel"}, CheckpointId: "checkpoint", DesiredVersion: 3}
			err := activateRestoredComputerOnSession(t.Context(), h, h, q)
			if (err == nil) != (name == "success") {
				t.Fatalf("error=%v", err)
			}
			want := []string{string(wire.StreamTypeComputerRestoreInstall)}
			if name != "altered install" {
				want = append(want, "ack")
			}
			if name == "success" {
				want = append(want, string(wire.StreamTypeComputerRestoreActivate))
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if !reflect.DeepEqual(h.calls, want) {
				t.Fatalf("calls=%v want=%v", h.calls, want)
			}
			if name != "altered install" && h.ack.Grants == nil {
				t.Fatal("idle restore omitted explicit empty grants")
			}
		})
	}
}

type restorePlanHarness struct {
	*restoreActivationHarness
	plan        *workerapi.ComputerRestorePlan
	planCalls   int
	lostAck     bool
	firstGrants []workerapi.ComputerRestoreGrant
}

func (h *restorePlanHarness) GetComputerRestorePlan(context.Context, workerapi.ComputerRestorePlanRequest) (workerapi.ComputerRestorePlanResponse, error) {
	h.planCalls++
	if h.planCalls == 1 {
		return workerapi.ComputerRestorePlanResponse{}, nil
	}
	return workerapi.ComputerRestorePlanResponse{Plan: h.plan}, nil
}
func (h *restorePlanHarness) AcknowledgeComputerRestore(ctx context.Context, q workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error) {
	response, err := h.restoreActivationHarness.AcknowledgeComputerRestore(ctx, q)
	if !h.lostAck {
		h.lostAck = true
		h.firstGrants = append([]workerapi.ComputerRestoreGrant(nil), q.Grants...)
		return workerapi.ComputerRestoreAckResponse{}, errors.New("response lost after commit")
	}
	if !reflect.DeepEqual(h.firstGrants, q.Grants) {
		return workerapi.ComputerRestoreAckResponse{}, errors.New("retry changed committed grants")
	}
	return response, err
}
func TestMaterializerRestoreRetriesSameInstallationAfterAckLoss(t *testing.T) {
	expiry := time.Now().Add(time.Minute)
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", ComputerID: "computer", RestoreCheckpointID: "checkpoint", WriterGeneration: 2, RuntimeEpoch: 3, VMPlatformID: "platform", DesiredVersion: 4, GuestdChannelToken: "channel", ExpiresAt: time.Now().Add(-time.Second)}
	h := &restorePlanHarness{restoreActivationHarness: &restoreActivationHarness{}, plan: &workerapi.ComputerRestorePlan{ComputerInstanceID: "instance", ComputerID: "computer", CheckpointID: "checkpoint", WriterGeneration: 2, WorkerEpoch: 3, VMPlatformID: "platform", DesiredVersion: 4, WriteCapability: "capability", WorkerHostID: "worker", Members: []workerapi.ComputerRestoreMember{{RunID: "run", AttemptNumber: 1, Lease: workerapi.RunLeaseFence{ID: "lease", LeaseSequence: 2}, ExpiresAt: expiry}}}}
	if err := (ComputerMaterializer{RestoreControl: h}).activateRestore(t.Context(), h, mount); err != nil {
		t.Fatal(err)
	}
	if h.planCalls != 2 {
		t.Fatalf("plan refetched after installation: calls=%d", h.planCalls)
	}
	want := []string{string(wire.StreamTypeComputerRestoreInstall), "ack", string(wire.StreamTypeComputerRestoreInstall), "ack", string(wire.StreamTypeComputerRestoreActivate)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("calls=%v", h.calls)
	}
}
