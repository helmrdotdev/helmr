package guestd

import (
	"errors"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAbsentSessionRetirementFencesDelayedStart(t *testing.T) {
	s, _ := agentSessionFixture(t)
	r := newComputerOperationRegistry()
	r.entries[s.entry.computerInstanceID] = s.entry
	request := &agentv1.SessionAttach{Grant: s.grant, AttachmentSequence: 1}
	if _, err := r.openAgentSession(t.Context(), request, nil); !errors.Is(err, errAgentSessionAbsent) {
		t.Fatalf("ordinary absence: %v", err)
	}
	if len(r.agentSessions) != 0 {
		t.Fatal("ordinary absence retired the epoch")
	}
	request.RetireAbsent = true
	relay, err := r.openAgentSession(t.Context(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !relay.session.physicalClosed || relay.session.process != nil || !relay.session.terminal {
		t.Fatal("retirement created a process or failed to fence")
	}
	select {
	case <-relay.finished:
	default:
		t.Fatal("cleanup pending for absent process")
	}
	relay.finish(errors.New("later Computer cleanup"))
	if len(relay.outbox) != 1 || relay.outbox[0].GetStopped() == nil {
		t.Fatal("retirement receipt not retained")
	}
	for _, buffer := range relay.session.logs {
		record, ok := buffer.peek()
		if !ok || record.Kind != diagnosticEnd || !record.Complete {
			t.Fatal("empty diagnostics not completed")
		}
		if err := buffer.acknowledge(record.Through); err != nil {
			t.Fatal(err)
		}
	}
	request.AttachmentSequence++
	again, err := r.openAgentSession(t.Context(), request, nil)
	if err != nil || again != relay {
		t.Fatalf("retirement replay: %v", err)
	}
	stale := proto.Clone(request).(*agentv1.SessionAttach)
	stale.RetireAbsent = false
	stale.Start = s.start
	retained, err := r.openAgentSession(t.Context(), stale, nil)
	if err != nil || retained != relay {
		t.Fatalf("delayed start did not retain fenced epoch: %v", err)
	}
	// A start frame cannot attach to the terminal-only relay with invented setup.
	if err := retained.attach(nil, stale); err == nil {
		t.Fatal("delayed startup accepted")
	}
	next := proto.Clone(request).(*agentv1.SessionAttach)
	next.Grant.Identity.ProcessEpoch++
	next.RetireAbsent = false
	if _, err := r.openAgentSession(t.Context(), next, nil); !errors.Is(err, errAgentSessionAbsent) {
		t.Fatalf("fresh epoch blocked by retired predecessor: %v", err)
	}
	if s.entry.active != 0 {
		t.Fatal("absent epoch retained a mount claim")
	}
}

func TestAbsentRetirementPreservesExistingProcess(t *testing.T) {
	s, p := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), s, nil)
	r := newComputerOperationRegistry()
	r.agentSessions = map[string]*agentRelay{s.grant.Identity.SessionId: relay}
	got, err := r.openAgentSession(t.Context(), &agentv1.SessionAttach{Grant: s.grant, AttachmentSequence: 1, RetireAbsent: true}, nil)
	if err != nil || got != relay || s.physicalClosed || s.process != p {
		t.Fatalf("existing process was replaced: %v", err)
	}
}

func TestAbsentRetirementRejectsStartupMaterial(t *testing.T) {
	for _, mode := range []string{"start", "secrets", "environment", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := agentSessionFixture(t)
			r := newComputerOperationRegistry()
			r.entries[s.entry.computerInstanceID] = s.entry
			request := &agentv1.SessionAttach{Grant: s.grant, AttachmentSequence: 1, RetireAbsent: true}
			switch mode {
			case "start":
				request.Start = s.start
			case "secrets":
				request.Secrets = []*agentv1.SessionSecret{{Value: []byte("fixture")}}
			case "environment":
				request.ProtectedEnv = map[string]string{"FIXTURE": "value"}
			case "proxy":
				request.ProxyCa = []byte("fixture")
			}
			if _, err := r.openAgentSession(t.Context(), request, nil); err == nil {
				t.Fatal("retirement accepted startup material")
			}
			if len(r.agentSessions) != 0 {
				t.Fatal("invalid request retired epoch")
			}
		})
	}
}

func TestAbsentRetirementCaptureWaitsForReceipt(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	existing := r.agentSessions["session"].session
	grant := proto.Clone(existing.grant).(*agentv1.SessionGrant)
	grant.Identity.SessionId = "retired"
	relay := newRetiredAgentRelay(t.Context(), existing.entry, grant)
	r.agentSessions["retired"] = relay
	if _, err := r.captureAgentComputer(t.Context(), request); err == nil {
		t.Fatal("capture discarded unobserved stop")
	}
	relay.mu.Lock()
	relay.acknowledged = 1
	relay.pruneLocked()
	relay.mu.Unlock()
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatalf("retired epoch blocked capture after receipt: %v", err)
	}
}
