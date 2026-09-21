//go:build linux

package firecracker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/sirupsen/logrus"
)

// The API boundary is fake; these checks do not qualify VMM/device durability.
func TestSnapshotFailureNeverResumesGuest(t *testing.T) {
	for _, stage := range []string{"pause response lost", "snapshot rejected", "invalid runtime identity", "invalid manifest", "missing backing file"} {
		t.Run(stage, func(t *testing.T) {
			api := &snapshotFailureAPI{}
			switch stage {
			case "pause response lost":
				api.pauseErr = context.DeadlineExceeded
			case "snapshot rejected":
				api.snapshotErr = errors.New("snapshot failed")
			}
			root := t.TempDir()
			client := sdk.NewClient(filepath.Join(root, "api.sock"), logrus.NewEntry(logrus.New()), false, sdk.WithOpsClient(api))
			machine, err := sdk.NewMachine(context.Background(), sdk.Config{SocketPath: filepath.Join(root, "api.sock")}, sdk.WithClient(client))
			if err != nil {
				t.Fatal(err)
			}
			session := &guestSession{machine: machine, jailRoot: root}
			if stage == "invalid manifest" || stage == "missing backing file" {
				session.runtimeIdentity = testRuntimeIdentity(t, testDigest([]byte("kernel")), testDigest([]byte("initramfs")), testDigest([]byte("rootfs")))
			}
			if stage == "missing backing file" {
				session.cfg = testRestoreConfig(t)
				session.cpuConfigDigest = testCPUConfigDigest(session.cfg.VCPUCount)
				session.kernelArgs = runtimeKernelArgs(vm.RuntimeTopology{}, nil, session.cfg.NetworkResolverIPv4)
				session.scratchDisk = filepath.Join(root, "missing-scratch.ext4")
			}

			_, err = session.CreateSnapshot(context.Background(), vm.SnapshotRequest{ID: "checkpoint"})
			if err == nil {
				t.Fatal("expected capture failure")
			}
			if stage == "missing backing file" && !strings.Contains(err.Error(), "pack checkpoint") {
				t.Fatalf("did not reach file packing: %v", err)
			}
			if stage == "invalid manifest" && !strings.Contains(err.Error(), "manifest") {
				t.Fatalf("did not reach manifest validation: %v", err)
			}
			if !api.paused || api.resumes != 0 {
				t.Fatalf("paused=%v resumes=%d", api.paused, api.resumes)
			}
			if api.pauseErr != nil && api.snapshots != 0 {
				t.Fatal("snapshot attempted after ambiguous pause")
			}
		})
	}
}

type snapshotFailureAPI struct {
	operations.ClientIface
	pauseErr, snapshotErr error
	paused                bool
	resumes, snapshots    int
}

func (a *snapshotFailureAPI) PatchVM(p *operations.PatchVMParams) (*operations.PatchVMNoContent, error) {
	if *p.Body.State == models.VMStatePaused {
		a.paused = true // Applied even when its response is lost.
		return &operations.PatchVMNoContent{}, a.pauseErr
	}
	a.resumes++
	a.paused = false
	return &operations.PatchVMNoContent{}, nil
}

func (a *snapshotFailureAPI) CreateSnapshot(*operations.CreateSnapshotParams) (*operations.CreateSnapshotNoContent, error) {
	a.snapshots++
	return &operations.CreateSnapshotNoContent{}, a.snapshotErr
}
