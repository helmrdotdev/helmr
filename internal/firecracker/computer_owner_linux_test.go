//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
)

type ownedComputerFixture struct {
	excluded <-chan struct{}
	closeErr error
	closes   int
	failure  chan error
}

func (d *ownedComputerFixture) BindConsumer(excluded <-chan struct{}) error {
	if d.excluded != nil {
		return errors.New("already bound")
	}
	d.excluded = excluded
	return nil
}
func (d *ownedComputerFixture) LinkInto(context.Context, string, int, int) (string, error) {
	return "", errors.New("not used")
}
func (d *ownedComputerFixture) Wait(ctx context.Context) error {
	select {
	case err := <-d.failure:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (d *ownedComputerFixture) Close(context.Context) error {
	select {
	case <-d.excluded:
	default:
		return errors.New("release before exclusion")
	}
	d.closes++
	return d.closeErr
}

func TestComputerDeviceCleanupRetainsFailedOwner(t *testing.T) {
	ctx := t.Context()
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: "01992000-0000-7000-8000-000000000002"}
	c := &Connector{cfg: Config{StateDir: t.TempDir(), JailerChrootBaseDir: t.TempDir(), IPPath: "/bin/true"}, computerDevices: &sync.Map{}}
	state, err := createOwnerStateRoot(c.cfg.StateDir, owner)
	if err != nil {
		t.Fatal(err)
	}
	device := &ownedComputerFixture{closeErr: errors.New("helper still alive")}
	retained := c.lockComputerOwner(owner)
	if err := retainComputerDevice(retained, device); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(state, "disk"))
	if err != nil {
		t.Fatal(err)
	}
	retained.files = map[string]*os.File{"computer": file}
	retained.mu.Unlock()
	// The per-launch copy and original reconciler share the same owner.
	child := *c
	if err := os.WriteFile(filepath.Join(state, "owner"), []byte("wrong owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := child.cleanup(ctx, owner); err == nil {
		t.Fatal("accepted missing exact ownership proof")
	}
	if device.closes != 0 {
		t.Fatal("released without physical cleanup")
	}
	select {
	case <-device.excluded:
		t.Fatal("signalled exclusion prematurely")
	default:
	}
	if err := os.WriteFile(filepath.Join(state, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := child.cleanup(ctx, owner); err == nil {
		t.Fatal("ignored helper release failure")
	}
	if _, ok := c.computerDevices.Load(owner); !ok {
		t.Fatal("lost retry owner")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("host disk handle remains: %v", err)
	}
	if err := validateOwnerMarker(state, owner); err != nil {
		t.Fatalf("removed recovery evidence: %v", err)
	}
	retained.mu.Lock()
	err = retainComputerDevice(retained, &ownedComputerFixture{})
	retained.mu.Unlock()
	if err == nil {
		t.Fatal("rebound unreleased runtime")
	}
	device.closeErr = nil
	if err := c.cleanup(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if device.closes != 2 {
		t.Fatalf("release attempts = %d", device.closes)
	}
	if _, ok := c.computerDevices.Load(owner); ok {
		t.Fatal("retained released owner")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("state not removed: %v", err)
	}
}

func TestComputerExportFailureCancelsLaunchContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cause := errors.New("object authentication failed")
	device := &ownedComputerFixture{failure: make(chan error, 1)}
	watch := watchComputerExport(ctx, cancel, device)
	device.failure <- cause
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("VMM launch context remains live")
	}
	if err := watch.join(); !errors.Is(err, cause) {
		t.Fatalf("lost export cause: %v", err)
	}
}

func TestComputerExportWatchJoinsNormalStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	watch := watchComputerExport(ctx, cancel, &ownedComputerFixture{failure: make(chan error)})
	cancel()
	if err := watch.join(); err != nil {
		t.Fatal(err)
	}
}

func TestComputerSnapshotDropsDeviceCapability(t *testing.T) {
	copy := cloneRuntimeComputer(&vm.RuntimeComputer{Device: &ownedComputerFixture{}})
	if copy.Device != nil {
		t.Fatal("snapshot retains live device authority")
	}
}

func TestComputerCleanupWaitsForStartupBeforeDeviceBinding(t *testing.T) {
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: "01992000-0000-7000-8000-000000000003"}
	c := &Connector{cfg: Config{StateDir: t.TempDir(), JailerChrootBaseDir: t.TempDir(), IPPath: "/bin/true"}, computerDevices: &sync.Map{}}
	// Startup holds the same guard before state creation or device registration.
	retained := c.lockComputerOwner(owner)
	finished := make(chan error, 1)
	go func() { finished <- c.cleanup(t.Context(), owner) }()
	select {
	case err := <-finished:
		t.Fatalf("cleanup bypassed startup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := createOwnerStateRoot(c.cfg.StateDir, owner); err != nil {
		t.Fatal(err)
	}
	device := &ownedComputerFixture{}
	if err := retainComputerDevice(retained, device); err != nil {
		t.Fatal(err)
	}
	retained.mu.Unlock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join startup")
	}
	if device.closes != 1 {
		t.Fatal("cleanup missed late device registration")
	}
}
