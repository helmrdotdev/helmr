package deployment

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
	ResolveBootstrapPort = uint32(5002)
	ResolveHostPort      = uint32(5003)

	resolveOperation = byte(0x01)
	relayTokenBytes  = 32
)

var (
	resolveBootstrapMagic = [4]byte{'H', 'R', 'B', '0'}
	resolveBootstrapAck   = [4]byte{'H', 'R', 'A', '0'}
)

type RelayCapability [relayTokenBytes]byte

func prepareResolveRelay(
	ctx context.Context,
	session vm.PreparedSession,
	random io.Reader,
) (RelayCapability, vm.HostEndpoint, error) {
	if session == nil {
		return RelayCapability{}, nil, errors.New("resolve prepared session is nil")
	}
	if random == nil {
		random = rand.Reader
	}
	var token RelayCapability
	if _, err := io.ReadFull(random, token[:]); err != nil {
		return RelayCapability{}, nil, fmt.Errorf(
			"generate resolve relay capability: %w",
			err,
		)
	}
	endpoint, err := session.BindHost(ctx, ResolveHostPort)
	if err != nil {
		clearRelayCapability(&token)
		return RelayCapability{}, nil, fmt.Errorf("bind resolve relay endpoint: %w", err)
	}
	if err := writeResolveBootstrap(ctx, session, token); err != nil {
		clearRelayCapability(&token)
		return RelayCapability{}, nil, errors.Join(err, endpoint.Close())
	}
	return token, endpoint, nil
}

func clearRelayCapability(capability *RelayCapability) {
	if capability == nil {
		return
	}
	for index := range capability {
		capability[index] = 0
	}
}

func writeResolveBootstrap(
	ctx context.Context,
	session vm.PreparedSession,
	token RelayCapability,
) error {
	stream, err := session.DialGuest(ctx, ResolveBootstrapPort)
	if err != nil {
		return fmt.Errorf("connect resolve bootstrap: %w", err)
	}
	stopClose := closeOnCancellation(ctx, stream)
	defer stopClose()
	defer stream.Close()

	var request [4 + 1 + relayTokenBytes]byte
	copy(request[:4], resolveBootstrapMagic[:])
	request[4] = resolveOperation
	copy(request[5:], token[:])
	if _, err := stream.Write(request[:]); err != nil {
		return fmt.Errorf(
			"write resolve bootstrap: %w",
			preferContextError(ctx, err),
		)
	}
	if err := stream.CloseWrite(); err != nil {
		return fmt.Errorf(
			"half-close resolve bootstrap: %w",
			preferContextError(ctx, err),
		)
	}
	var response [len(resolveBootstrapAck)]byte
	if _, err := io.ReadFull(stream, response[:]); err != nil {
		return fmt.Errorf(
			"read resolve bootstrap acknowledgement: %w",
			preferContextError(ctx, err),
		)
	}
	if subtle.ConstantTimeCompare(
		response[:],
		resolveBootstrapAck[:],
	) != 1 {
		return errors.New("resolve bootstrap acknowledgement is invalid")
	}
	var trailing [1]byte
	if count, err := stream.Read(trailing[:]); err != io.EOF || count != 0 {
		if err == nil {
			err = errors.New("trailing data")
		}
		return fmt.Errorf(
			"resolve bootstrap acknowledgement is not terminated: %w",
			preferContextError(ctx, err),
		)
	}
	if !stopClose() {
		return preferContextError(ctx, io.ErrClosedPipe)
	}
	return nil
}

func AcceptResolveBootstrap(
	conn io.ReadWriter,
) (RelayCapability, error) {
	if conn == nil {
		return RelayCapability{}, errors.New("resolve bootstrap connection is nil")
	}
	var request [4 + 1 + relayTokenBytes]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil {
		return RelayCapability{}, fmt.Errorf("read resolve bootstrap: %w", err)
	}
	var trailing [1]byte
	if count, err := conn.Read(trailing[:]); err != io.EOF || count != 0 {
		if err == nil {
			err = errors.New("trailing data")
		}
		return RelayCapability{}, fmt.Errorf(
			"resolve bootstrap is not terminated: %w",
			err,
		)
	}
	if subtle.ConstantTimeCompare(
		request[:4],
		resolveBootstrapMagic[:],
	) != 1 {
		return RelayCapability{}, errors.New("resolve bootstrap magic is invalid")
	}
	if request[4] != resolveOperation {
		return RelayCapability{}, errors.New("resolve bootstrap operation is invalid")
	}
	var token RelayCapability
	copy(token[:], request[5:])
	if _, err := conn.Write(resolveBootstrapAck[:]); err != nil {
		return RelayCapability{}, fmt.Errorf(
			"write resolve bootstrap acknowledgement: %w",
			err,
		)
	}
	return token, nil
}
