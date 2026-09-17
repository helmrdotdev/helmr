//go:build linux

package firecracker

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/firecracker-microvm/firecracker-go-sdk"
)

func TestStartMachineContextJoinsCanceledStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requestCtx, cancelRequest := context.WithCancel(t.Context())
		defer cancelRequest()
		machineCtx, cancelMachine := context.WithCancel(context.Background())
		defer cancelMachine()
		entered, release := make(chan struct{}), make(chan struct{})
		finished := false
		machine := startupTestMachine(t, machineCtx, func(ctx context.Context, _ *firecracker.Machine) error {
			close(entered)
			<-ctx.Done()
			<-release // Cancellation is cooperative; cleanup must wait for this handler.
			finished = true
			return ctx.Err()
		})
		result := make(chan error, 1)
		go func() { result <- startMachineContext(requestCtx, machine, machineCtx, cancelMachine) }()
		<-entered
		cancelRequest()
		synctest.Wait()
		if machineCtx.Err() != context.Canceled {
			t.Error("request cancellation did not cancel machine startup")
		}
		select {
		case err := <-result:
			t.Errorf("startup returned before handler exit: %v", err)
			close(release)
			synctest.Wait()
			return
		default:
		}
		close(release)
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error = %v, want request cancellation", err)
		}
		// The result must synchronize all handler writes with caller cleanup.
		if !finished {
			t.Fatal("handler work was not joined before cleanup")
		}
	})
}

func TestStartMachineContextCancelsOnStartupError(t *testing.T) {
	machineCtx, cancelMachine := context.WithCancel(context.Background())
	defer cancelMachine()
	want := errors.New("synthetic SDK handler failure")
	machine := startupTestMachine(t, machineCtx, func(context.Context, *firecracker.Machine) error { return want })
	if err := startMachineContext(t.Context(), machine, machineCtx, cancelMachine); !errors.Is(err, want) {
		t.Fatalf("startup error = %v, want %v", err, want)
	}
	if machineCtx.Err() != context.Canceled {
		t.Fatal("failed startup retained a live machine context")
	}
}

func TestStartMachineContextSuccessfulHandoffOwnsLifetime(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	machineCtx, cancelMachine := context.WithCancel(context.Background())
	defer cancelMachine()
	var handlerCtx context.Context
	machine := startupTestMachine(t, machineCtx, func(ctx context.Context, _ *firecracker.Machine) error {
		handlerCtx = ctx
		return nil
	})
	if err := startMachineContext(requestCtx, machine, machineCtx, cancelMachine); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if handlerCtx != machineCtx || handlerCtx.Err() != nil {
		t.Fatal("successful startup inherited request lifetime")
	}
	cancelMachine()
	if handlerCtx.Err() != context.Canceled {
		t.Fatal("machine owner cannot cancel the SDK context")
	}
}

// Exercise the real SDK Start/handler path without launching a VMM. A paused
// snapshot suppresses Start's final InstanceStart API call; its load handler is
// replaced by the controlled handler. This does not prove VM process lifetime.
func startupTestMachine(t *testing.T, ctx context.Context, fn func(context.Context, *firecracker.Machine) error) *firecracker.Machine {
	t.Helper()
	machine, err := firecracker.NewMachine(ctx, firecracker.Config{DisableValidation: true},
		withSnapshotRestore("unused.mem", "unused.state"),
		func(machine *firecracker.Machine) {
			machine.Handlers.FcInit = firecracker.HandlerList{}.Append(firecracker.Handler{Name: "test.Startup", Fn: fn})
		})
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
