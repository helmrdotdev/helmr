package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const (
	managerAcquireGuestTTL     = 5 * time.Minute
	managerAcquireCloseTimeout = 30 * time.Second
	managerAcquirePIDsMax      = int64(128)
)

type ManagerNormalizer interface {
	Normalize(
		context.Context,
		ManagerSelector,
		ManagerSource,
		*os.File,
		*os.File,
	) (ManagerAcquireTerminal, error)
}

type GuestManagerNormalizer struct {
	Connector vm.Connector
}

func (normalizer GuestManagerNormalizer) Normalize(
	ctx context.Context,
	selector ManagerSelector,
	source ManagerSource,
	archive *os.File,
	provisional *os.File,
) (terminal ManagerAcquireTerminal, returnErr error) {
	if normalizer.Connector == nil {
		return ManagerAcquireTerminal{}, errors.New("manager acquisition guest connector is required")
	}
	if ctx == nil {
		return ManagerAcquireTerminal{}, errors.New("manager acquisition context is nil")
	}
	request := ManagerAcquireRequest{
		Architecture:   selector.Architecture,
		FormatVersion:  ManagerAcquireFormatVersion,
		PackageManager: selector.PackageManager,
		Source: ManagerAcquireSource{
			Digest:    source.Digest,
			SizeBytes: source.SizeBytes,
		},
	}
	if err := validateManagerArchive(archive, request); err != nil {
		return ManagerAcquireTerminal{}, fmt.Errorf("validate manager acquisition source: %w", err)
	}
	if err := validateManagerAcquireFile(provisional, "provisional output"); err != nil {
		return ManagerAcquireTerminal{}, err
	}
	requestBody, err := CanonicalManagerAcquireRequest(request)
	if err != nil {
		return ManagerAcquireTerminal{}, err
	}
	bodyLen := uint64(4+len(requestBody)) + uint64(source.SizeBytes)
	if bodyLen > ManagerAcquireMaxInputBytes {
		return ManagerAcquireTerminal{}, errors.New("manager acquisition input exceeds the protocol limit")
	}

	guestCtx, cancel := context.WithTimeout(ctx, managerAcquireGuestTTL)
	defer cancel()
	session, err := normalizer.Connector.Connect(guestCtx, vm.ConnectRequest{
		OwnerKind:   vm.OwnerBuild,
		Resources:   compute.ManagerAcquireResources(),
		PIDsMax:     managerAcquirePIDsMax,
		Networkless: true,
	})
	if err != nil {
		return ManagerAcquireTerminal{}, vm.NewGuestError(
			fmt.Errorf("connect manager acquisition guest: %w", err),
		)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(
			context.Background(),
			managerAcquireCloseTimeout,
		)
		defer closeCancel()
		if err := session.Close(closeCtx); err != nil {
			returnErr = errors.Join(
				returnErr,
				vm.NewGuestError(fmt.Errorf("close manager acquisition guest: %w", err)),
			)
		}
	}()

	stream := session.Stream()
	stopClose := closeOnCancellation(guestCtx, stream)
	defer stopClose()
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{Type: wire.StreamTypeManagerAcquire},
		bodyLen,
	); err != nil {
		return ManagerAcquireTerminal{}, vm.NewGuestError(
			fmt.Errorf("write manager acquisition stream header: %w", preferContextError(guestCtx, err)),
		)
	}
	if err := WriteManagerAcquireRequest(stream, request, archive); err != nil {
		return ManagerAcquireTerminal{}, vm.NewGuestError(
			fmt.Errorf("write manager acquisition request: %w", preferContextError(guestCtx, err)),
		)
	}
	terminal, err = ReadManagerAcquireResponse(stream, provisional, request)
	if err != nil {
		return ManagerAcquireTerminal{}, vm.NewGuestError(
			fmt.Errorf("read manager acquisition response: %w", preferContextError(guestCtx, err)),
		)
	}
	if !stopClose() {
		return ManagerAcquireTerminal{}, vm.NewGuestError(
			preferContextError(guestCtx, io.ErrClosedPipe),
		)
	}
	return terminal, nil
}
