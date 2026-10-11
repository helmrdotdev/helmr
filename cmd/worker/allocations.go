package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func newAllocationConsumer(machines *computerhost.PreparedMachines, client *workerclient.Client, limit int, poll time.Duration, admission *worker.HardAdmission) (*worker.AllocationConsumer, error) {
	if machines == nil || client == nil || admission == nil {
		return nil, errors.New("allocation machines and authenticated client are required")
	}
	return worker.NewAllocationConsumer(client, limit, poll, func(identity workerapi.AllocationIdentity, admitStart func(context.Context) error) (worker.AllocationOwner, error) {
		underlyingAdmission := admitStart
		admitStart = func(ctx context.Context) error {
			if admission.CheckpointKeyUnavailable() {
				return computerhost.ErrCheckpointKeyUnavailable
			}
			err := underlyingAdmission(ctx)
			if admission.CheckpointKeyUnavailable() {
				return computerhost.ErrCheckpointKeyUnavailable
			}
			return err
		}
		// The supervisor authenticates, recovers and activates before discovery.
		host, epoch, err := client.HostIdentity()
		if err != nil {
			return nil, err
		}
		switch identity.Kind {
		case "computer":
			return computerhost.NewComputerAllocationOwner(machines, client, identity, host, epoch, admitStart, admission.PauseForCheckpointKeyMismatch, unexpectedAllocationEvent)
		case "preparation":
			return computerhost.NewPreparationAllocationOwner(machines, client, identity, epoch, admitStart)
		default:
			return nil, fmt.Errorf("unsupported allocation kind %q", identity.Kind)
		}
	})
}

// Execution, controls, diagnostics and operations have durable owners in the
// Session service. Never acknowledge an event that none of those owners handled.
func unexpectedAllocationEvent(_ context.Context, message *agentv1.GuestSessionMessage) error {
	return fmt.Errorf("unhandled allocation Session event %T", message.GetMessage())
}
