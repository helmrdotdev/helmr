package guestd

import (
	"bytes"
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"testing"
)

func restoreSetFixture(t *testing.T) (*computerOperationRegistry, *waitingRunRegistry, *computerMountEntry, *computerv0.MaterializeComputerRequest) {
	r, w, identity := frozenComputerFixture(t, 2)
	entry := r.entries["mounted"]
	delete(r.entries, "mounted")
	r.entries[entry.computerInstanceID] = entry
	entry.baseComputerDiskVersionID = "source-version"
	entry.computerMount = "/computer"
	entry.channelToken = "source-token"
	r.captureRequest = &computerv0.FreezeComputerRequest{ComputerId: identity.ComputerId, ComputerInstanceId: identity.SourceComputerInstanceId, WriterGeneration: 3, CheckpointId: identity.CheckpointId, DesiredVersion: 2, MembershipRevision: 2}
	for _, m := range identity.Runs {
		r.captureRequest.Runs = append(r.captureRequest.Runs, &computerv0.ComputerCaptureRun{RunId: m.RunId, AttemptNumber: m.AttemptNumber, RunWaitId: m.RunWaitId, RunLeaseId: m.RunLeaseId})
	}
	request := &computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: identity.ComputerId, ComputerInstanceId: "destination", WriterGeneration: 4, ChannelToken: "destination-token"}, MountPath: "/computer", Target: &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: "destination-version"}, RestoredCheckpointId: identity.CheckpointId, UsePreparedRuntime: true}
	return r, w, entry, request
}

func TestRestoreMaterializationValidatesAllFrozenProgramsBeforeRebind(t *testing.T) {
	changes := map[string]func(*computerOperationRegistry, *waitingRunRegistry, *computerv0.MaterializeComputerRequest){
		"missing claim": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			r.programClaims = r.programClaims[:1]
		},
		"missing wait": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			delete(w.slots, "wait-b")
		},
		"unfrozen peer": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			w.slots["wait-b"].frozen = false
		},
		"peer checkpoint": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			w.slots["wait-b"].checkpointID = "other"
		},
		"peer lease": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			r.programClaims[1].authority.Fence.RunLeaseId = "other"
		},
		"duplicate capture member": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			r.captureRequest.Runs[1] = r.captureRequest.Runs[0]
		},
		"unsealed": func(r *computerOperationRegistry, w *waitingRunRegistry, q *computerv0.MaterializeComputerRequest) {
			r.captureRequest = nil
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, w, entry, q := restoreSetFixture(t)
			change(r, w, q)
			if _, err := r.materializeRestoredComputerMount(q, w); err == nil {
				t.Fatal("incomplete capture rebound")
			}
			if entry.computerInstanceID != "source-instance" || entry.currentWriterGeneration() != 3 || entry.channelToken != "source-token" || entry.baseComputerDiskVersionID != "source-version" || r.restoredMaterialization != nil {
				t.Fatal("rejected restore mutated physical authority")
			}
		})
	}
}

func TestRestoreMaterializationHandlerRebindsSetWithoutReleasingPrograms(t *testing.T) {
	r, w, entry, q := restoreSetFixture(t)
	before := proto.Clone(r.captureRequest).(*computerv0.FreezeComputerRequest)
	call := func(q *computerv0.MaterializeComputerRequest) error {
		var body bytes.Buffer
		if err := frameio.WriteProtoFrame(&body, q); err != nil {
			t.Fatal(err)
		}
		stream := &scriptedNetConn{reader: bytes.NewReader(body.Bytes())}
		return handleComputerMaterializeConnection(context.Background(), stream, slogDiscard(), r, w)
	}
	if err := call(q); err != nil {
		t.Fatal(err)
	}
	if err := call(q); err != nil {
		t.Fatal(err)
	}
	if r.entries["destination"] != entry || r.entries["source-instance"] != nil || !proto.Equal(r.captureRequest, before) {
		t.Fatal("rebind identity/barrier mismatch")
	}
	for _, claim := range r.programClaims {
		if claim.authority.Fence.ComputerInstanceId != "source-instance" || claim.authority.Fence.WriterGeneration != 3 {
			t.Fatal("materialization granted Program authority")
		}
	}
	for _, slot := range w.slots {
		if !slot.frozen || slot.granted != nil || slot.accepted != nil {
			t.Fatal("materialization released frozen Program")
		}
	}
	changed := proto.Clone(q).(*computerv0.MaterializeComputerRequest)
	changed.Target.BaseComputerDiskVersionId = "different-destination"
	if _, err := r.materializeRestoredComputerMount(changed, w); err == nil {
		t.Fatal("changed replay accepted")
	}
	q.Envelope.ChannelToken = "mutated-caller"
	if r.restoredMaterialization.Envelope.ChannelToken != "destination-token" {
		t.Fatal("receipt aliases request")
	}
}

func TestPreparedOnlyRestoreTransfersFilesystemOwnershipOnce(t *testing.T) {
	r, w, identity := frozenComputerFixture(t, 0)
	prepared := r.preparedRuntime
	prepared.computerMount = "/computer"
	prepared.computerRoot = t.TempDir()
	prepared.imageRoot = "retained-image"
	cleaned := 0
	prepared.cleanup = func() { cleaned++ }
	r.captureRequest = &computerv0.FreezeComputerRequest{ComputerId: identity.ComputerId, ComputerInstanceId: identity.SourceComputerInstanceId, WriterGeneration: 3, CheckpointId: identity.CheckpointId, DesiredVersion: 1}
	q := &computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: identity.ComputerId, ComputerInstanceId: "destination", WriterGeneration: 4, ChannelToken: "new-token"}, MountPath: "/computer", Target: &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: "restored-base"}, RestoredCheckpointId: identity.CheckpointId, UsePreparedRuntime: true}
	invalid := proto.Clone(q).(*computerv0.MaterializeComputerRequest)
	invalid.Envelope.WriterGeneration = 3
	if _, err := r.materializeRestoredComputerMount(invalid, w); err == nil {
		t.Fatal("non-advancing writer accepted")
	}
	if r.preparedRuntime != prepared || len(r.entries) != 0 || cleaned != 0 {
		t.Fatal("failed restore consumed retained filesystem")
	}
	for range 2 {
		if _, err := r.materializeRestoredComputerMount(q, w); err != nil {
			t.Fatal(err)
		}
	}
	entry := r.entries["destination"]
	if r.preparedRuntime != nil || entry == nil || entry.computerRoot != prepared.computerRoot || entry.imageRoot != prepared.imageRoot || entry.currentWriterGeneration() != 4 || cleaned != 0 {
		t.Fatal("filesystem ownership was not transferred intact")
	}
	if !r.captureSealed() || len(r.programClaims) != 0 || len(w.slots) != 0 {
		t.Fatal("empty restore opened admission or synthesized Program")
	}
	r.retire("destination", entry)
	r.retire("destination", entry)
	if cleaned != 1 {
		t.Fatalf("cleanup count=%d", cleaned)
	}
}
