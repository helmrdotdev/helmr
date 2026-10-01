package computerhost

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"uuid"
)

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
	mount.GuestChannelCredential = "test-channel"
	mount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte(mount.GuestChannelCredential))
	go acknowledgePreparedComputerMount(t, ps, mount, mount.ComputerInstanceID)
	raw := &serverTestMachine{streams: []io.ReadWriteCloser{pc}, operation: discardReadWriteCloser{}, exit: make(chan error)}
	machines := computerPreparedMachines(t, mount, raw)
	client := &serverTestClient{}
	m := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{runtime: mount.ComputerInstanceID, computer: mount.ComputerID}, ComputerSaveEvery: time.Millisecond, ComputerObjects: &checkpointCAS{}, CAS: store, TempDir: t.TempDir(), Heartbeat: time.Hour, PollEvery: time.Hour, Machines: machines, Mounts: NewMounts()}
	err := m.Serve(ctx, mount, client)
	if err == nil || !strings.Contains(err.Error(), errTestLiveCapture.Error()) {
		t.Fatalf("original failure lost: %v", err)
	}
	if len(client.failures) != 1 || !strings.Contains(string(client.failures[0].Error), "computer_preservation_failed") || !strings.Contains(string(client.failures[0].Error), errTestLiveCapture.Error()) {
		t.Fatalf("failure not reported: %+v", client.failures)
	}
	// The save loop captures through the machine the machines admitted.
	if raw.captureCount() == 0 {
		t.Fatalf("live captures=%d", raw.captureCount())
	}
	if raw.closeCount() != 1 {
		t.Fatalf("runtime cleanup=%d", raw.closeCount())
	}
}
