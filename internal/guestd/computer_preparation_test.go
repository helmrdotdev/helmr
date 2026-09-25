package guestd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

func TestComputerPreparationUsesMountedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", root)
	marker := filepath.Join(root, "customer-state")
	if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.PrepareWorkspaceRuntimeRequest{MountedImageConfig: &workspacev0.RuntimeImageConfig{WorkingDir: "/project", User: "1000:1000"}}
	image, cleanup, err := restorePreparedWorkspaceImage(strings.NewReader("not an image stream"), request)
	if err != nil {
		t.Fatal(err)
	}
	if image.RootfsDir != root || image.Config.WorkingDir != "/project" || image.Config.User != "1000:1000" {
		t.Fatalf("wrong mounted environment: %+v", image)
	}
	cleanup()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup removed customer disk: %v", err)
	}
	request.MountedImageConfig = nil
	if _, _, err := restorePreparedWorkspaceImage(strings.NewReader(""), request); err == nil || !strings.Contains(err.Error(), "admitted image config") {
		t.Fatalf("missing config tried stream fallback: %v", err)
	}
}
