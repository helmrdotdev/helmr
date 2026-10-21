package computerhost

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/helmrdotdev/helmr/internal/oci"
	"sync"
	"sync/atomic"
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
	return runPrivatePreparation(ctx, machine, allocation, credential, start.Seed.Config, baseVersion, &o.expires, &o.workWake, func(ctx context.Context) (*computerv0.PreparationStart, error) {
		if qualification {
			return &computerv0.PreparationStart{QualificationOnly: true, LogLimits: o.machines.PreparationLogLimits}, nil
		}
		secrets, err := o.client.PreparationSecrets(ctx, o.executor)
		if err != nil {
			return nil, err
		}
		request := &computerv0.PreparationStart{TemplateDefinitionId: start.TemplateDefinitionID, LogLimits: o.machines.PreparationLogLimits, ProtectedEnv: secrets.Protected.Values(), ProxyCa: secrets.Protected.PublicCA()}
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
				return nil, errors.New("preparation secret requires exactly one placement")
			}
			request.Secrets = append(request.Secrets, delivery)
		}
		return request, nil
	}, func(code string) {
		if qualification && (code == "preparation_execution_failed" || code == "preparation_scope_termination_failed") {
			o.failureCode = "preparation_execution_failed"
		}
	})
}

func runPrivatePreparation(ctx context.Context, machine vm.CheckpointableMachine, allocation workerapi.AllocationIdentity, credential []byte, config oci.RuntimeConfig, baseVersion string, expires *atomic.Int64, wake *allocationWake, startup func(context.Context) (*computerv0.PreparationStart, error), failed func(string)) error {
	// These are mounted-image substrate identities. There is no Session or Run
	// admission in a private preparation VM.
	if err := prepareAllocationImage(ctx, machine, allocation, base64.RawURLEncoding.EncodeToString(credential), baseVersion, config); err != nil {
		return err
	}
	identity := &computerv0.PreparationIdentity{PreparationId: allocation.OwnerID, InstanceId: allocation.AllocationID, Epoch: allocation.Epoch, ChannelCredential: credential}
	// Lease delivery and execution state progress independently of both diagnostic
	// streams. No log receipt gates the preparation result or subsequent capture.
	ctx, cancel := context.WithCancel(ctx)
	var renewal sync.WaitGroup
	renewal.Go(func() { renewPrivatePreparationGuest(ctx, machine, identity, expires) })
	defer func() { cancel(); renewal.Wait() }()
	request := &computerv0.PreparationControlRequest{Identity: identity}
	started := false
	for {
		preparationWake := wake.snapshot()
		request.ExpiresAtUnixNano = expires.Load()
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
			request.Start, err = startup(ctx)
			if err != nil {
				return err
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
			failed(response.ErrorCode)
			return errors.New("guest preparation failed: " + response.ErrorCode)
		}

	}
}

func renewPrivatePreparationGuest(ctx context.Context, machine vm.CheckpointableMachine, identity *computerv0.PreparationIdentity, expires *atomic.Int64) {
	for {
		remaining := time.Until(time.Unix(0, expires.Load()))
		if remaining <= 0 || sleepWithContext(ctx, min(10*time.Second, max(100*time.Millisecond, remaining/2), remaining)) != nil {
			return
		}
		callCtx, stop := context.WithDeadline(ctx, minTime(time.Unix(0, expires.Load()), time.Now().Add(5*time.Second)))
		request := &computerv0.PreparationControlRequest{Identity: identity, ExpiresAtUnixNano: expires.Load(), RenewOnly: true}
		var response computerv0.PreparationControlResponse
		// The guest still expires locally if transport is unavailable. This loop
		// neither starts authored work nor advances the output acknowledgment cursor.
		_ = machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
			return allocationGuestExchange(ctx, machine, wire.StreamTypePreparationControl, request, &response)
		})
		stop()
	}
}
