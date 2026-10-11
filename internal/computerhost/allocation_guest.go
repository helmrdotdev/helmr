package computerhost

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/oci"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func prepareAllocationImage(ctx context.Context, machine vm.GuestControlMachine, identity workerapi.AllocationIdentity, channelCredential, baseVersion string, config oci.RuntimeConfig) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return machine.WithRunningGuestControl(ctx, vm.GuestControlOrdinary, func(ctx context.Context) error {
		prepared := &computerv0.PrepareComputerRuntimeRequest{ComputerInstanceId: identity.InstanceID, ComputerId: identity.OwnerID, WriterGeneration: identity.Epoch, MountPath: "/workspace", MountedImageConfig: &computerv0.RuntimeImageConfig{Env: config.Env, WorkingDir: config.WorkingDir, User: config.User, Entrypoint: config.Entrypoint, Cmd: config.Cmd}}
		var ready computerv0.PrepareComputerRuntimeResponse
		if err := allocationGuestExchange(ctx, machine, wire.StreamTypeComputerRuntimePrepare, prepared, &ready); err != nil {
			return err
		}
		if ready.GetStatus() != "prepared" || ready.GetComputerInstanceId() != identity.InstanceID {
			return errors.New("computer preparation receipt differs from its allocation")
		}
		request := &computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: identity.OwnerID, ComputerInstanceId: identity.InstanceID, WriterGeneration: uint64(identity.Epoch), ChannelCredential: channelCredential}, MountPath: "/workspace", Target: &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: baseVersion}}
		var response computerv0.MaterializeComputerResponse
		if err := allocationGuestExchange(ctx, machine, wire.StreamTypeComputerMaterialize, request, &response); err != nil {
			return err
		}
		if response.GetStatus() != "running" || response.GetGuestChannelCredentialHash() != sha256sum.HexBytes([]byte(channelCredential)) || !proto.Equal(response.GetTarget(), request.GetTarget()) {
			return errors.New("computer materialization receipt differs from its allocation")
		}
		return nil
	})
}

func allocationGuestExchange(ctx context.Context, machine vm.Machine, kind wire.StreamType, request, response proto.Message) error {
	stream, err := machine.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if _, err := writeGuestControlRequest(stream, wire.StreamHeader{Type: kind}, request); err != nil {
		return err
	}
	return frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, response)
}
