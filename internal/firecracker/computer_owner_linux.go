//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/helmrdotdev/helmr/internal/vm"
)

// Retained across failed startup and failed Cleanup, including when no Session
// could be returned. Per-launch Connector copies share this ownership table.
type computerDeviceOwner struct {
	mu        sync.Mutex
	device    vm.ComputerDevice
	excluded  chan struct{}
	exclusion sync.Once
	files     map[string]*os.File
}

// The guard is installed before startup creates state, including for cleanup
// that arrives before launch. Recheck after locking: successful cleanup can
// remove a guard while another caller was waiting for it.
func (c *Connector) lockComputerOwner(owner vm.Owner) *computerDeviceOwner {
	if c.computerDevices == nil {
		return nil
	}
	for {
		value, _ := c.computerDevices.LoadOrStore(owner, &computerDeviceOwner{})
		retained := value.(*computerDeviceOwner)
		retained.mu.Lock()
		if current, ok := c.computerDevices.Load(owner); ok && current == retained {
			return retained
		}
		retained.mu.Unlock()
	}
}

// The caller holds the launch guard. A failed bind transfers no ownership.
func retainComputerDevice(retained *computerDeviceOwner, device vm.ComputerDevice) error {
	if retained == nil {
		return errors.New("computer device ownership is not configured")
	}
	if retained.device != nil {
		return errors.New("runtime already owns a computer device")
	}
	excluded := make(chan struct{})
	if err := device.BindConsumer(excluded); err != nil {
		return err
	}
	retained.device, retained.excluded = device, excluded
	return nil
}

// Only cleanup, after physical exclusion, may call this with retained locked.
func (c *Connector) releaseComputerDevice(ctx context.Context, owner vm.Owner, retained *computerDeviceOwner) error {
	if retained == nil || retained.device == nil {
		return nil
	}
	if err := closeRuntimeDiskFiles(retained.files); err != nil {
		return cleanupUnproven(owner, err)
	}
	retained.exclusion.Do(func() { close(retained.excluded) })
	if err := retained.device.Close(ctx); err != nil {
		return cleanupUnproven(owner, fmt.Errorf("release computer device: %w", err))
	}
	return nil
}

type computerExportWatch struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func watchComputerExport(ctx context.Context, stop context.CancelFunc, device vm.ComputerDevice) *computerExportWatch {
	watch := &computerExportWatch{done: make(chan struct{})}
	go func() {
		defer close(watch.done)
		err := device.Wait(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("export ended without a cause")
		}
		watch.mu.Lock()
		watch.err = fmt.Errorf("computer export failed: %w", err)
		watch.mu.Unlock()
		// Cancel the actual SDK process context even during Start or guest health
		// checks. Cancellation is not exclusion; Cleanup still proves absence.
		stop()
	}()
	return watch
}

func (watch *computerExportWatch) failure() error {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	return watch.err
}

func (watch *computerExportWatch) join() error {
	<-watch.done
	return watch.failure()
}
