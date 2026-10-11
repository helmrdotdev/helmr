package computerhost

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// One loop per pipe retains the exact head through uncertain CP responses. The
// guest releases it only after the authenticated host forwards durable acceptance
// or an explicit expired disposition. Neither loop owns preparation lifecycle.
func (o *PreparationAllocationOwner) deliverPreparationLogs(ctx context.Context, stream string) {
	request := &computerv0.PreparationControlRequest{Identity: &computerv0.PreparationIdentity{PreparationId: o.identity.OwnerID, InstanceId: o.identity.InstanceID, Epoch: o.identity.Epoch, ChannelCredential: o.executor.ChannelCredential}, LogStream: stream}
	for ctx.Err() == nil {
		request.ExpiresAtUnixNano = o.expires.Load()
		var response computerv0.PreparationControlResponse
		callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		err := o.machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
			return allocationGuestExchange(ctx, o.machine, wire.StreamTypePreparationControl, request, &response)
		})
		stop()
		if err != nil || response.Log == nil {
			if sleepWithContext(ctx, time.Second) != nil {
				return
			}
			continue
		}
		log := response.Log
		if wire.ValidatePreparationLog(log, o.machines.PreparationLogLimits.GetChunkBytes()) != nil || log.Sequence != request.AcknowledgedThrough+1 {
			slog.Warn("preparation diagnostic envelope rejected", "preparation_id", o.identity.OwnerID, "stream", stream)
			return
		}
		upload := workerapi.PreparationLogRequest{Executor: o.executor, Stream: stream, Kind: log.Kind, Sequence: log.Sequence, ThroughSequence: log.ThroughSequence, ObservedAtUnixNano: log.ObservedAtUnixNano, Data: log.Data, DroppedBytes: log.DroppedBytes, Complete: log.Complete}
		for {
			callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			receipt, err := o.client.AppendPreparationLog(callCtx, upload)
			stop()
			if err == nil {
				if receipt.ThroughSequence != log.ThroughSequence || (receipt.Expired && (!receipt.AcceptedAt.IsZero() || !receipt.ExpiresAt.IsZero())) || (!receipt.Expired && (receipt.AcceptedAt.IsZero() || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour)) {
					slog.Warn("preparation diagnostic receipt rejected", "preparation_id", o.identity.OwnerID, "stream", stream)
					return
				}
				request.AcknowledgedThrough = receipt.ThroughSequence
				break
			}
			var response *httpclient.Error
			if errors.As(err, &response) && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 429 {
				slog.Warn("preparation diagnostic stream rejected", "preparation_id", o.identity.OwnerID, "stream", stream)
				return
			}
			if sleepWithContext(ctx, time.Second) != nil {
				return
			}
		}
		if log.Kind == "end" {
			// The durable boundary already exists. Forwarding this final ACK only
			// releases guest memory; physical cleanup never depends on its delivery.
			request.ExpiresAtUnixNano = o.expires.Load()
			callCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			_ = o.machine.WithRunningGuestControl(callCtx, vm.GuestControlOrdinary, func(ctx context.Context) error {
				return allocationGuestExchange(ctx, o.machine, wire.StreamTypePreparationControl, request, &response)
			})
			stop()
			return
		}
	}
}
