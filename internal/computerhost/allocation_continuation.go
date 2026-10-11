package computerhost

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// A fresh physical observation permits refreshing grants only before guest
// installation. Once installed, the exact owner survives uncertain replies and
// renews attachments until activation/reconciliation have both completed.
func (o *ComputerAllocationOwner) continueCheckpoint(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, capture *agentv1.ComputerSessionCapture, sourceAbort bool) (resultErr error) {
	var owner *AgentComputerOwner
	defer func() {
		if owner != nil {
			resultErr = errors.Join(resultErr, owner.Close())
		}
	}()
	for {
		var observed *agentv1.ComputerSessionReceipt
		var err error
		if capture != nil {
			err = retryCheckpoint(ctx, func(ctx context.Context) error {
				var err error
				observed, err = controlAgentComputer(ctx, o.machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Inspect{Inspect: capture}})
				return err
			})
			if err != nil {
				return err
			}
		}
		if owner != nil && observed != nil && !observed.GetInstalled() {
			// Inspection can overtake an earlier Install whose reply was lost.
			// Retain its exact request until admission expires, then obtain a
			// new observation before deciding that replacement is safe.
			expires := time.Unix(0, owner.installation.GetEnvelope().GetOperationExpiresAtUnixNano())
			if delay := time.Until(expires); delay > 0 {
				if err = sleepWithContext(ctx, delay); err != nil {
					return err
				}
			}
			err = retryCheckpoint(ctx, func(ctx context.Context) error {
				var err error
				observed, err = controlAgentComputer(ctx, o.machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Inspect{Inspect: capture}})
				return err
			})
			if err != nil {
				return err
			}
		}
		if observed == nil || !observed.GetInstalled() {
			if owner != nil {
				if err = owner.Close(); err != nil {
					return err
				}
				owner = nil
			}
			installation, err := o.prepareContinuation(ctx, delivery, capture, observed, sourceAbort)
			if err != nil {
				if !checkpointContinuationRetryable(err) {
					return err
				}
				if err = sleepWithContext(ctx, 250*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			capture = installation.GetCapture()
			owner, err = NewAgentComputerOwner(ctx, o.machine, o.client, o.identity.EnvironmentID, installation, o.observe)
			if err != nil {
				return err
			}
		} else if owner == nil {
			return errors.New("installed Computer continuation has no retained request")
		}
		receipt, err := owner.Continue(ctx)
		if err == nil {
			return nil
		}
		// A validated guest rejection before installation can reflect expiring grants.
		// It supplies no execution authority: the next iteration inspects again.
		if !checkpointContinuationRetryable(err) && !(receipt != nil && !receipt.GetInstalled() && receipt.GetError() != "" && receipt.GetErrorCode() == agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_AUTHORITY_EXPIRED) {
			return err
		}
		if err = sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

func checkpointContinuationRetryable(err error) bool {
	var rejected *httpclient.Error
	return saveAdmissionRetryable(err) || (errors.As(err, &rejected) && rejected.Code == workerapi.AgentComputerNotReady)
}

func (o *ComputerAllocationOwner) prepareContinuation(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, capture *agentv1.ComputerSessionCapture, observed *agentv1.ComputerSessionReceipt, sourceAbort bool) (*agentv1.ComputerSessionInstallation, error) {
	var response workerapi.AgentComputerInstallationResponse
	var err error
	if sourceAbort {
		raw, err := proto.Marshal(observed)
		if err != nil {
			return nil, err
		}
		response, err = o.client.PrepareAgentComputerSourceAbort(ctx, workerapi.AgentComputerSourceAbortRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: capture.GetCheckpointId(), LeaseEpoch: o.identity.Epoch, ChannelCredential: delivery.ChannelCredential, Receipt: raw})
		if err != nil {
			return nil, err
		}
	} else {
		response, err = o.client.PrepareAgentComputerRestore(ctx, workerapi.AgentComputerRestoreRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: o.checkpoint.CheckpointID.String(), LeaseEpoch: o.identity.Epoch, ChannelCredential: delivery.ChannelCredential})
		if err != nil {
			return nil, err
		}
	}
	p := new(agentv1.ComputerSessionInstallation)
	if err = proto.Unmarshal(response.Installation, p); err != nil {
		return nil, err
	}
	e := p.GetEnvelope()
	if p.GetSourceAbort() != sourceAbort || e.GetComputerId() != o.identity.OwnerID || e.GetComputerInstanceId() != o.identity.InstanceID || e.GetWriterGeneration() != uint64(o.identity.Epoch) || e.GetChannelCredential() != delivery.ChannelCredential || p.GetBaseComputerDiskVersionId() != delivery.BaseVersion || (capture != nil && !proto.Equal(p.GetCapture(), capture)) || (!sourceAbort && p.GetCapture().GetCheckpointId() != o.checkpoint.CheckpointID.String()) {
		return nil, errors.New("computer installation differs from physical allocation")
	}
	if sourceAbort {
		raw, err := proto.Marshal(observed)
		if err != nil {
			return nil, err
		}
		// Validate reserves same-source continuation before settling its unattempted
		// disk cut. If delay expires these grants, reinspection prepares fresh ones.
		if err = o.client.ValidateAgentComputerSourceAbort(ctx, workerapi.AgentComputerInstallationRequest{EnvironmentID: o.identity.EnvironmentID, Installation: response.Installation, Receipt: raw}); err != nil {
			return nil, err
		}
		if err = o.client.AgentComputerSaveAbsence(ctx, workerapi.AgentComputerSaveAbsenceRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: capture.GetCheckpointId(), Evidence: "uncut:" + capture.GetCheckpointId()}); err != nil {
			return nil, err
		}
	}
	return p, nil
}
