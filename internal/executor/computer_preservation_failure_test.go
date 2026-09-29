package executor

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"uuid"
)

type saveFailureRegistry struct {
	ComputerMountSessionRegistry
	t *testing.T
}

func (r saveFailureRegistry) RegisterComputerMountSession(m workerapi.ComputerInstanceAssignment, s vm.Machine, _ string) func() {
	return func() {}
}
func TestPreservationFailureSurvivesRenewalCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pc, ps := net.Pipe()
	defer ps.Close()
	store, mount := testComputerMountArtifacts(t)
	mount.ComputerID = uuid.NewV7().String()
	mount.ComputerInstanceID = uuid.NewV7().String()
	mount.ComputerInstanceID = uuid.NewV7().String()
	mount.OrgID = uuid.NewV7().String()
	mount.GuestdChannelToken = "test-channel"
	mount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte(mount.GuestdChannelToken))
	go acknowledgePreparedComputerMount(t, ps, mount, mount.ComputerInstanceID)
	raw := &computerMaterializerTestSession{streams: []io.ReadWriteCloser{pc}, operation: discardReadWriteCloser{}, exit: make(chan error)}
	pool := computerPreparedRuntimePool(t, mount, raw)
	client := &computerMaterializerTestClient{}
	m := ComputerMaterializer{ComputerSaves: &saveHostFixture{runtime: mount.ComputerInstanceID, computer: mount.ComputerID}, ComputerSaveEvery: time.Millisecond, ComputerObjects: &checkpointCAS{}, CAS: store, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, RuntimePool: pool, Sessions: saveFailureRegistry{t: t}}
	err := m.RunComputerMount(ctx, mount, client)
	if err == nil || !strings.Contains(err.Error(), "runtime cannot capture a live Computer") {
		t.Fatalf("original failure lost: %v", err)
	}
	if len(client.failures) != 1 || !strings.Contains(string(client.failures[0].Error), "computer_preservation_failed") || !strings.Contains(string(client.failures[0].Error), "runtime cannot capture a live Computer") {
		t.Fatalf("failure not reported: %+v", client.failures)
	}
	if raw.closeCount() != 1 {
		t.Fatalf("runtime cleanup=%d", raw.closeCount())
	}
}
