//go:build linux

package executor

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestExecComputerGenerationSurvivesSourceClose(t *testing.T) {
	_, lease, session, store, cfg := newTerminalCaptureTest(t)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	session.checkpointSession.stream = host
	want := bytes.Repeat([]byte{29}, 4096)
	if _, err := session.disk.WriteAt(t.Context(), want, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		if _, _, err := wire.ReadStreamFrameHeader(guest); err != nil {
			done <- err
			return
		}
		var request workspacev0.StopWorkspaceRequest
		if err := frameio.ReadProtoFrame(guest, &request); err != nil {
			done <- err
			return
		}
		done <- frameio.WriteProtoFrame(guest, &workspacev0.StopWorkspaceResponse{Status: "stopped"})
	}()
	client := &workspaceMaterializerTestClient{}
	store.retry = true
	err := (WorkspaceMaterializer{ComputerObjects: store}).stopControlledWorkspaceMount(t.Context(), session, workerapi.WorkspaceMount{ID: "mount", OrgID: "org", WorkspaceID: lease.WorkspaceID, GuestdChannelToken: "token", FencingGeneration: 2}, workerapi.WorkspaceMountResponse{Status: "unmounting", FinalizationKind: "capture", FencingGeneration: 2}, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if session.closeCount != 1 || client.stops != 1 || len(client.captures) != 1 || store.uploads < 2 {
		t.Fatal("capture/retry/close/settlement not completed")
	}
	if err := os.RemoveAll(cfg.Directory); err != nil {
		t.Fatal(err)
	}
	cfg.Directory = filepath.Join(t.TempDir(), "restored")
	cfg.Base = client.captures[0].Computer.Root
	restored, err := computer.CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got := make([]byte, len(want))
	if _, err := restored.ReadAt(t.Context(), got, 0); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("restore failed: %v", err)
	}
}
