package computerhost

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (o *PreparationAllocationOwner) runGuestPreparation(ctx context.Context, start workerapi.PreparationStart) error {
	return o.runPreparationOn(ctx, o.machine, o.identity, o.executor.ChannelCredential, start, start.Seed.ArtifactDigest, false)
}

func (o *PreparationAllocationOwner) runPreparationOn(ctx context.Context, machine vm.CheckpointableMachine, allocation workerapi.AllocationIdentity, credential []byte, start workerapi.PreparationStart, baseVersion string, qualification bool) error {
	// These are mounted-image substrate identities. There is no Session or Run
	// admission in a private preparation VM.
	if err := prepareAllocationImage(ctx, machine, allocation, base64.RawURLEncoding.EncodeToString(credential), baseVersion, start.Seed.Config); err != nil {
		return err
	}
	identity := &computerv0.PreparationIdentity{PreparationId: allocation.OwnerID, InstanceId: allocation.AllocationID, Epoch: allocation.Epoch, ChannelCredential: credential}
	// Lease delivery and execution state progress independently of both diagnostic
	// streams. No log receipt gates the preparation result or subsequent capture.
	ctx, cancel := context.WithCancel(ctx)
	var renewal sync.WaitGroup
	renewal.Go(func() { o.renewPreparationGuest(ctx, machine, identity) })
	defer func() { cancel(); renewal.Wait() }()
	request := &computerv0.PreparationControlRequest{Identity: identity}
	started := false
	for {
		preparationWake := o.workWake.snapshot()
		request.ExpiresAtUnixNano = o.expires.Load()
		if err := wire.ValidatePreparationControl(request); err != nil {
			for _, secret := range request.GetStart().GetSecrets() {
				clear(secret.Value)
			}
			return err
		}
		request.WaitForCompletion = started && request.Start == nil
		callCtx, stop := context.WithTimeout(ctx, 35*time.Second)
		var response computerv0.PreparationControlResponse
		var err error
		if request.WaitForCompletion {
			// This observes the retained execution only. Do not hold the VM control
			// barrier while waiting; guest lease renewals must remain independent.
			err = allocationGuestExchange(callCtx, machine, wire.StreamTypePreparationControl, request, &response)
		} else {
			err = machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
				return allocationGuestExchange(ctx, machine, wire.StreamTypePreparationControl, request, &response)
			})
		}
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
			if waitAllocationWake(ctx, preparationWake, time.Second) != nil {
				return errors.Join(ctx.Err(), err)
			}
			continue
		}
		switch response.State {
		case "absent":
			if started {
				return errors.New("preparation startup outcome is unknown; authored code will not be repeated")
			}
			if qualification {
				request.Start = &computerv0.PreparationStart{QualificationOnly: true, LogLimits: o.machines.PreparationLogLimits}
				continue
			}
			secrets, err := o.client.PreparationSecrets(ctx, o.executor)
			if err != nil {
				return err
			}
			request.Start = &computerv0.PreparationStart{TemplateDefinitionId: start.TemplateDefinitionID, LogLimits: o.machines.PreparationLogLimits, ProtectedEnv: secrets.Protected.Values(), ProxyCa: secrets.Protected.PublicCA()}
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
			if qualification && (response.ErrorCode == "preparation_execution_failed" || response.ErrorCode == "preparation_scope_termination_failed") {
				o.failureCode = "preparation_execution_failed"
			}
			return errors.New("guest preparation failed: " + response.ErrorCode)
		}

	}
}

func (o *PreparationAllocationOwner) renewPreparationGuest(ctx context.Context, machine vm.CheckpointableMachine, identity *computerv0.PreparationIdentity) {
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
		_ = machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
			return allocationGuestExchange(ctx, machine, wire.StreamTypePreparationControl, request, &response)
		})
		stop()
	}
}
