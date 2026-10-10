package computerhost

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (o *PreparationAllocationOwner) runGuestPreparation(ctx context.Context, start workerapi.PreparationStart) error {
	// These are mounted-image substrate identities. There is no Session or Run
	// admission in a private preparation VM.
	if err := prepareAllocationImage(ctx, o.machine, o.identity, base64.RawURLEncoding.EncodeToString(o.executor.ChannelCredential), start.Seed.ArtifactDigest, start.Seed.Config); err != nil {
		return err
	}
	identity := &computerv0.PreparationIdentity{PreparationId: o.identity.OwnerID, InstanceId: o.identity.InstanceID, Epoch: o.identity.Epoch, ChannelCredential: o.executor.ChannelCredential}
	// Lease delivery and execution state progress independently of both diagnostic
	// streams. No log receipt gates the preparation result or subsequent capture.
	ctx, cancel := context.WithCancel(ctx)
	var renewal sync.WaitGroup
	renewal.Go(func() { o.renewPreparationGuest(ctx, identity) })
	defer func() { cancel(); renewal.Wait() }()
	request := &computerv0.PreparationControlRequest{Identity: identity}
	started := false
	for {
		request.ExpiresAtUnixNano = o.expires.Load()
		if err := wire.ValidatePreparationControl(request); err != nil {
			for _, secret := range request.GetStart().GetSecrets() {
				clear(secret.Value)
			}
			return err
		}
		callCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		var response computerv0.PreparationControlResponse
		err := o.machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
			return allocationGuestExchange(ctx, o.machine, wire.StreamTypePreparationControl, request, &response)
		})
		stop()
		// Only the first confirmed absence permits transmission of authored startup.
		// After an uncertain send, probe the same retained owner without new secrets.
		if request.Start != nil {
			for _, secret := range request.Start.Secrets {
				clear(secret.Value)
			}
			request.Start = nil
			started = true
		}
		if err != nil {
			if sleepWithContext(ctx, time.Second) != nil {
				return errors.Join(ctx.Err(), err)
			}
			continue
		}
		switch response.State {
		case "absent":
			if started {
				return errors.New("preparation startup outcome is unknown; authored code will not be repeated")
			}
			secrets, err := o.client.PreparationSecrets(ctx, o.executor)
			if err != nil {
				return err
			}
			request.Start = &computerv0.PreparationStart{ComputerDefinitionId: start.ComputerDefinitionID, LogLimits: o.machines.PreparationLogLimits, ProtectedEnv: secrets.Protected.Values(), ProxyCa: secrets.Protected.PublicCA()}
			for _, secret := range secrets.Secrets {
				delivery := &computerv0.ComputerSecretDelivery{Value: secret.Value}
				switch {
				case secret.Env != nil && secret.File == nil:
					delivery.PlacementKind, delivery.PlacementTarget = "env", secret.Env.Name
				case secret.File != nil && secret.Env == nil:
					delivery.PlacementKind, delivery.PlacementTarget = "file", secret.File.Path
				default:
					for _, secret := range secrets.Secrets {
						clear(secret.Value)
					}
					return errors.New("preparation secret requires exactly one placement")
				}
				request.Start.Secrets = append(request.Start.Secrets, delivery)
			}
			continue
		case "running", "succeeded", "failed":
			started = true
		default:
			return errors.New("invalid preparation guest state")
		}
		if response.State == "succeeded" {
			return nil
		}
		if response.State == "failed" {
			return errors.New("guest preparation failed: " + response.ErrorCode)
		}
		if err := sleepWithContext(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (o *PreparationAllocationOwner) renewPreparationGuest(ctx context.Context, identity *computerv0.PreparationIdentity) {
	for {
		remaining := time.Until(time.Unix(0, o.expires.Load()))
		if remaining <= 0 || sleepWithContext(ctx, min(10*time.Second, max(100*time.Millisecond, remaining/2), remaining)) != nil {
			return
		}
		callCtx, stop := context.WithDeadline(ctx, minTime(time.Unix(0, o.expires.Load()), time.Now().Add(5*time.Second)))
		request := &computerv0.PreparationControlRequest{Identity: identity, ExpiresAtUnixNano: o.expires.Load(), RenewOnly: true}
		var response computerv0.PreparationControlResponse
		// The guest still expires locally if transport is unavailable. This loop
		// neither starts authored work nor advances the output acknowledgment cursor.
		_ = o.machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
			return allocationGuestExchange(ctx, o.machine, wire.StreamTypePreparationControl, request, &response)
		})
		stop()
	}
}

func flushPreparation(ctx context.Context, machine vm.GuestControlMachine, owner string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return machine.WithRunningGuestControl(ctx, vm.GuestControlOrdinary, func(ctx context.Context) error {
		stream, err := machine.OpenStream(ctx)
		if err != nil {
			return err
		}
		defer stream.Close()
		stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
		defer stop()
		operation := uuid.NewV7().String()
		if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{Type: wire.StreamTypeComputerFlush, ComputerID: owner, OperationID: operation}, 0); err != nil {
			return err
		}
		if err := frameio.WriteProtoFrame(stream, &computerv0.FlushComputerRequest{OperationId: operation}); err != nil {
			return err
		}
		var response computerv0.FlushComputerResponse
		if err := frameio.ReadProtoFrameBounded(stream, 4096, &response); err != nil {
			return err
		}
		if response.OperationId != operation || response.Error != "" {
			return errors.New("preparation filesystem flush was not acknowledged")
		}
		return nil
	})
}
