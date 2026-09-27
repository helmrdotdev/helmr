package guestd

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"google.golang.org/protobuf/proto"
)

func frozenComputerFixture(t *testing.T, count int) (*computerOperationRegistry, *waitingRunRegistry, *computerv0.ComputerRestoreIdentity) {
	t.Helper()
	mounts, waits := newComputerOperationRegistry(), newWaitingRunRegistry()
	identity := &computerv0.ComputerRestoreIdentity{ComputerId: "computer", SourceComputerInstanceId: "source-instance", WriterGeneration: 3, CheckpointId: "checkpoint"}
	if count == 0 {
		mounts.preparedRuntime = &preparedComputerRuntime{computerID: identity.ComputerId, computerInstanceID: identity.SourceComputerInstanceId, writerGeneration: 3}
		return mounts, waits, identity
	}
	entry := &computerMountEntry{computerID: identity.ComputerId, computerInstanceID: identity.SourceComputerInstanceId, writerGeneration: 3}
	mounts.entries["mounted"] = entry
	for i := range count {
		key := string(rune('a' + i))
		member := &computerv0.CapturedRun{RunId: "run-" + key, AttemptNumber: 2, RunWaitId: "wait-" + key, RunLeaseId: "lease-" + key, CorrelationId: "correlation-" + key}
		identity.Runs = append(identity.Runs, member)
		registration, err := waits.registerProgram(&programv0.CheckpointPauseRequest{RunId: member.RunId, AttemptNumber: member.AttemptNumber, RunWaitId: member.RunWaitId, RunLeaseId: member.RunLeaseId, CorrelationId: member.CorrelationId, CheckpointId: identity.CheckpointId, ResumeAttachId: "attach-" + key, CheckpointRequestVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		registration.markFrozen()
		mounts.programClaims = append(mounts.programClaims, &managedProgramClaim{entry: entry, authority: &computerv0.ComputerRunAuthority{Fence: &computerv0.ComputerAuthorityFence{ComputerId: identity.ComputerId, ComputerInstanceId: identity.SourceComputerInstanceId, WriterGeneration: 3, RunId: member.RunId, AttemptNumber: member.AttemptNumber, RunLeaseId: member.RunLeaseId}}})
	}
	return mounts, waits, identity
}

func TestComputerRestoreVerifiesCompleteFrozenSet(t *testing.T) {
	for _, count := range []int{0, 2} {
		mounts, waits, identity := frozenComputerFixture(t, count)
		var body bytes.Buffer
		if err := frameio.WriteProtoFrame(&body, &computerv0.VerifyComputerRestoreRequest{Identity: identity}); err != nil {
			t.Fatal(err)
		}
		stream := &scriptedNetConn{reader: bytes.NewReader(body.Bytes())}
		if err := handleComputerRestoreVerifyConnection(stream, 0, mounts, waits); err != nil {
			t.Fatalf("count=%d: %v", count, err)
		}
		var response computerv0.VerifyComputerRestoreResponse
		if err := frameio.ReadProtoFrame(bytes.NewReader(stream.written.Bytes()), &response); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(response.Identity, identity) {
			t.Fatal("verification changed captured identity")
		}
		// Verification grants no authority to execute.
		for _, slot := range waits.slots {
			if slot.accepted != nil || slot.granted != nil || slot.appliedDecision != nil {
				t.Fatal("verification activated a member")
			}
		}
	}
}

func TestComputerRestoreRejectsIncompleteOrChangedSet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*computerOperationRegistry, *waitingRunRegistry, *computerv0.ComputerRestoreIdentity)
	}{
		{"missing member", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.Runs = i.Runs[:1]
		}},
		{"duplicate member", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.Runs[1] = proto.Clone(i.Runs[0]).(*computerv0.CapturedRun)
		}},
		{"source", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.SourceComputerInstanceId = "other"
		}},
		{"generation", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.WriterGeneration++
		}},
		{"lease", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.Runs[0].RunLeaseId = "other"
		}},
		{"checkpoint", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.CheckpointId = "other"
		}},
		{"correlation", func(_ *computerOperationRegistry, _ *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			i.Runs[0].CorrelationId = "other"
		}},
		{"not frozen", func(_ *computerOperationRegistry, w *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			w.slots[i.Runs[0].RunWaitId].frozen = false
		}},
		{"resumed", func(_ *computerOperationRegistry, w *waitingRunRegistry, i *computerv0.ComputerRestoreIdentity) {
			w.slots[i.Runs[0].RunWaitId].accepted = &programv0.ResumeAttach{}
		}},
		{"missing claim", func(m *computerOperationRegistry, _ *waitingRunRegistry, _ *computerv0.ComputerRestoreIdentity) {
			m.programClaims = m.programClaims[:1]
		}},
		{"command", func(m *computerOperationRegistry, _ *waitingRunRegistry, _ *computerv0.ComputerRestoreIdentity) {
			m.entries["mounted"].commands = map[string]*computerBasicExec{"command": {}}
		}},
		{"admission in flight", func(m *computerOperationRegistry, _ *waitingRunRegistry, _ *computerv0.ComputerRestoreIdentity) {
			m.entries["mounted"].processAdmissions = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, w, i := frozenComputerFixture(t, 2)
			test.change(m, w, i)
			if err := verifyFrozenComputer(m, w, i); err == nil {
				t.Fatal("changed frozen guest accepted")
			}
		})
	}
}

func TestIdleRestoreRequiresCapturedPhysicalIdentity(t *testing.T) {
	for _, test := range []string{"missing", "computer", "instance", "generation", "unexpected run"} {
		t.Run(test, func(t *testing.T) {
			m, w, i := frozenComputerFixture(t, 0)
			switch test {
			case "missing":
				m.preparedRuntime = nil
			case "computer":
				m.preparedRuntime.computerID = "other"
			case "instance":
				m.preparedRuntime.computerInstanceID = "other"
			case "generation":
				m.preparedRuntime.writerGeneration++
			case "unexpected run":
				m.programClaims = append(m.programClaims, &managedProgramClaim{})
			}
			if err := verifyFrozenComputer(m, w, i); err == nil {
				t.Fatal("unrelated idle guest accepted")
			}
		})
	}
}
