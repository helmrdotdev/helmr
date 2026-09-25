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
	WorkspaceMountSessionRegistry
	t *testing.T
}

func (r saveFailureRegistry) RegisterWorkspaceMountSession(m workerapi.WorkspaceMount, s vm.Session, _ string) func() {
	managed := s.(*managedWorkspaceMountSession)
	detach, err := managed.saves.attach(m.RuntimeInstanceID, m.WorkspaceID, func() *workerapi.ComputerSaveBeginRequest { return &workerapi.ComputerSaveBeginRequest{} })
	if err != nil {
		r.t.Fatal(err)
	}
	return detach
}
func TestPreservationFailureSurvivesRenewalCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pc, ps := net.Pipe()
	defer ps.Close()
	store, mount := testWorkspaceMountArtifacts(t)
	mount.WorkspaceID = uuid.NewV7().String()
	mount.RuntimeInstanceID = uuid.NewV7().String()
	mount.ID = uuid.NewV7().String()
	mount.OrgID = uuid.NewV7().String()
	mount.GuestdChannelToken = "test-channel"
	mount.GuestdChannelTokenHash = sha256sum.HexBytes([]byte(mount.GuestdChannelToken))
	go acknowledgePreparedWorkspaceMount(t, ps, mount, mount.RuntimeInstanceID)
	raw := &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{pc}, operation: discardReadWriteCloser{}, exit: make(chan error)}
	pool := workspacePreparedRuntimePool(t, mount, raw)
	client := &workspaceMaterializerTestClient{}
	m := WorkspaceMaterializer{ComputerSaves: &saveHostFixture{runtime: mount.RuntimeInstanceID, computer: mount.WorkspaceID}, ComputerSaveEvery: time.Millisecond, ComputerObjects: &checkpointCAS{}, CAS: store, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, RuntimePool: pool, Sessions: saveFailureRegistry{t: t}}
	err := m.RunWorkspaceMount(ctx, mount, client)
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
