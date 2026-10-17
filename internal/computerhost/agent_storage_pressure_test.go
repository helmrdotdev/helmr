package computerhost

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type uncertainPressureClient struct {
	AgentSaveClient
	report func(workerapi.AgentStoragePressure) error
}

func (c uncertainPressureClient) ReportAgentStoragePressure(_ context.Context, r workerapi.AgentStoragePressure) error {
	return c.report(r)
}

func TestStoragePressureResolutionReconcilesUncertainActiveReport(t *testing.T) {
	saves, _ := agentSaveFixture(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var active atomic.Bool
	active.Store(true)
	wake := make(chan struct{}, 1)
	saves.changes = func() disk.VersionChanges {
		state := disk.VersionChanges{PressureWake: wake}
		if active.Load() {
			state.PressureSince = time.Now()
		}
		return state
	}
	calls := 0
	saves.client = uncertainPressureClient{report: func(r workerapi.AgentStoragePressure) error {
		calls++
		if calls == 1 {
			if !r.Active {
				t.Fatal("first report was not active")
			}
			active.Store(false)
			wake <- struct{}{}
			return io.ErrUnexpectedEOF
		}
		if r.Active {
			t.Fatal("resolved state retained active pressure")
		}
		cancel()
		return nil
	}}
	if err := saves.runPressure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("uncertain pressure was not resolved: calls=%d", calls)
	}
}
