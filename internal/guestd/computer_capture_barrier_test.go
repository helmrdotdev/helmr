package guestd

import (
	"strings"
	"sync"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func captureBarrierFixture(count int) (*computerOperationRegistry, *computerMountEntry, *computerv0.FreezeComputerRequest) {
	r := newComputerOperationRegistry()
	entry := &computerMountEntry{computerID: "computer", computerInstanceID: "instance", writerGeneration: 3, channelCredential: "token"}
	r.entries["instance"] = entry
	request := &computerv0.FreezeComputerRequest{ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 3, CheckpointId: "checkpoint", DesiredVersion: 5, MembershipRevision: 7}
	for i := range count {
		suffix := string(rune('a' + i))
		member := &computerv0.ComputerCaptureRun{RunId: "run-" + suffix, AttemptNumber: 2, RunWaitId: "wait-" + suffix, RunLeaseId: "lease-" + suffix}
		request.Runs = append(request.Runs, member)
		r.programClaims = append(r.programClaims, &managedProgramClaim{entry: entry, authority: &computerv0.ComputerRunAuthority{ChannelCredential: "token", Fence: &computerv0.ComputerAuthorityFence{ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 3, RunId: member.RunId, AttemptNumber: 2, RunLeaseId: member.RunLeaseId, ExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano()}}})
	}
	return r, entry, request
}

func TestComputerCaptureSealsCompleteClaims(t *testing.T) {
	for _, count := range []int{0, 2} {
		r, entry, request := captureBarrierFixture(count)
		if err := r.sealComputerCapture(request, time.Now); err != nil {
			t.Fatal(err)
		}
		if err := r.sealComputerCapture(proto.Clone(request).(*computerv0.FreezeComputerRequest), time.Now); err != nil {
			t.Fatal(err)
		}
		changed := proto.Clone(request).(*computerv0.FreezeComputerRequest)
		changed.DesiredVersion++
		if err := r.sealComputerCapture(changed, time.Now); err == nil {
			t.Fatal("barrier request replaced")
		}
		if !proto.Equal(r.captureRequest, request) {
			t.Fatal("barrier changed")
		}
		request.CheckpointId = "mutated"
		if r.captureRequest.CheckpointId != "checkpoint" {
			t.Fatal("barrier aliases caller")
		}
		if _, err := r.reserveMaterialization(); err == nil {
			t.Fatal("materialization admitted")
		}
		if err := r.setPreparedRuntime(&preparedComputerRuntime{}); err == nil {
			t.Fatal("preparation replaced source")
		}
		if err := r.register("mount", &computerMountEntry{}); err == nil {
			t.Fatal("mount replaced source")
		}
		authority := &computerv0.ComputerRunAuthority{ChannelCredential: "token", Fence: &computerv0.ComputerAuthorityFence{ComputerInstanceId: "instance", ComputerId: "computer"}}
		if _, err := r.admitProgram(entry, authority, time.Now); err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("Program admission: %v", err)
		}
		if _, _, err := r.startComputerBasicExec(t.Context(), entry, nil); err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("Command admission: %v", err)
		}
	}
}

func TestComputerCaptureRejectsChangedPhysicalSet(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*computerOperationRegistry, *computerMountEntry, *computerv0.FreezeComputerRequest)
	}{
		{"missing member", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			q.Runs = q.Runs[:1]
		}},
		{"duplicate member", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			q.Runs[1] = q.Runs[0]
		}},
		{"wrong lease", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			q.Runs[0].RunLeaseId = "other"
		}},
		{"expired claim", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			r.programClaims[0].authority.Fence.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
		}},
		{"wrong writer", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			q.WriterGeneration++
		}},
		{"other instance", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			q.ComputerInstanceId = "other"
		}},
		{"command", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			e.commands = map[string]*computerBasicExec{"command": {}}
		}},
		{"admitting process", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			e.processAdmissions = 1
		}},
		{"materializing", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			r.materializations = 1
		}},
		{"recovery", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			e.recoveryRequired = true
		}},
		{"stopping", func(r *computerOperationRegistry, e *computerMountEntry, q *computerv0.FreezeComputerRequest) {
			e.stopping = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, e, q := captureBarrierFixture(2)
			test.change(r, e, q)
			if err := r.sealComputerCapture(q, time.Now); err == nil {
				t.Fatal("unsafe capture sealed")
			}
			if r.captureSealed() {
				t.Fatal("failed validation changed admission")
			}
		})
	}
}

func TestPreparedComputerCaptureSealsWithoutProgram(t *testing.T) {
	r, _, q := captureBarrierFixture(0)
	r.entries = map[string]*computerMountEntry{}
	prepared := &preparedComputerRuntime{computerID: q.ComputerId, computerInstanceID: q.ComputerInstanceId, writerGeneration: q.WriterGeneration, computerImageDigest: "image", computerMount: "/workspace"}
	if err := r.setPreparedRuntime(prepared); err != nil {
		t.Fatal(err)
	}
	release, err := r.reserveMaterialization()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.sealComputerCapture(q, time.Now); err == nil {
		t.Fatal("capture raced filesystem mutation")
	}
	release()
	if err = r.sealComputerCapture(q, time.Now); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.takePreparedRuntime(q.ComputerInstanceId, q.ComputerId, "image", "/workspace", uint64(q.WriterGeneration)); ok {
		t.Fatal("sealed preparation consumed")
	}
	if r.preparedRuntime != prepared {
		t.Fatal("prepared identity lost")
	}
}

func TestComputerCaptureSerializesMountReplacement(t *testing.T) {
	for range 100 {
		r, _, q := captureBarrierFixture(0)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var captureErr, replaceErr error
		go func() { defer wg.Done(); <-start; captureErr = r.sealComputerCapture(q, time.Now) }()
		go func() {
			defer wg.Done()
			<-start
			replaceErr = r.register("mount", &computerMountEntry{computerID: "other", computerInstanceID: "other", writerGeneration: 4})
		}()
		close(start)
		wg.Wait()
		if captureErr == nil && replaceErr == nil {
			t.Fatal("both capture and replacement succeeded")
		}
		if captureErr == nil {
			if r.entries["instance"].computerID != "computer" {
				t.Fatal("captured source replaced")
			}
		} else if replaceErr != nil {
			t.Fatalf("neither operation succeeded: %v %v", captureErr, replaceErr)
		}
	}
}

func TestComputerCaptureChecksExpiryAfterLifecycleLock(t *testing.T) {
	r, entry, request := captureBarrierFixture(1)
	deadline := time.Now().Add(200 * time.Millisecond)
	r.programClaims[0].authority.Fence.ExpiresAtUnixNano = deadline.UnixNano()
	entry.lifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			entry.lifecycleMu.Unlock()
		}
	}()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); done <- r.sealComputerCapture(request, time.Now) }()
	<-started
	timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("capture skipped lifecycle lock: %v", err)
	case <-timer.C:
	}
	entry.lifecycleMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired claim sealed after lock contention")
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not finish after lock release")
	}
	if r.captureSealed() {
		t.Fatal("expired authority changed admission")
	}
}

func TestNewCaptureRetiresPreviousMaterializationReceipt(t *testing.T) {
	r, _, request := captureBarrierFixture(0)
	receipt := &computerv0.MaterializeComputerRequest{RestoredCheckpointId: "previous-checkpoint"}
	r.restoredMaterialization = receipt
	r.restoreActivated = true
	r.restoreInstallation = &computerv0.ComputerRestoreInstallation{DesiredVersion: request.DesiredVersion - 1}
	invalid := proto.Clone(request).(*computerv0.FreezeComputerRequest)
	invalid.WriterGeneration++
	if err := r.sealComputerCapture(invalid, time.Now); err == nil {
		t.Fatal("invalid capture accepted")
	}
	if r.restoredMaterialization != receipt {
		t.Fatal("rejected capture removed replay receipt")
	}
	if err := r.sealComputerCapture(request, time.Now); err != nil {
		t.Fatal(err)
	}
	if r.restoredMaterialization != nil {
		t.Fatal("new capture retained prior restore receipt")
	}
	r.restoredMaterialization = receipt
	if err := r.sealComputerCapture(request, time.Now); err != nil {
		t.Fatal(err)
	}
	if r.restoredMaterialization != receipt {
		t.Fatal("capture replay cleared receipt for its restored destination")
	}
}
