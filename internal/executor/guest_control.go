package executor

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

// guestControl sends host-to-guest Computer control operations. Each operation
// opens its own guest stream, writes one stream header and one request frame,
// and reads one response frame. Closing that stream never closes the machine.
type guestControl struct {
	machine vm.Machine
}

// guestControlStep names the exchange step that failed, so an operation can
// annotate its own transport failures.
type guestControlStep uint8

const (
	guestControlOpen guestControlStep = iota
	guestControlWriteHeader
	guestControlWriteRequest
	guestControlReadResponse
)

// guestControlCloseOnCancel selects whether context cancellation closes the
// operation stream while the exchange is in progress.
type guestControlCloseOnCancel uint8

const (
	// guestControlCloseOnCancelNone leaves header and request writes unaffected
	// by cancellation.
	guestControlCloseOnCancelNone guestControlCloseOnCancel = iota
	// guestControlCloseOnCancelAsync closes the stream when ctx is done at any
	// step, without waiting for that close before returning.
	guestControlCloseOnCancelAsync
	// guestControlCloseOnCancelAwait closes the stream when ctx is done at any
	// step and returns only after that close has finished.
	guestControlCloseOnCancelAwait
)

type guestControlExchange struct {
	header   wire.StreamHeader
	request  proto.Message
	response proto.Message
	// closeOnCancel selects how cancellation reaches the stream during the
	// whole exchange.
	closeOnCancel guestControlCloseOnCancel
	// readWithContext makes the response read return ctx.Err() on
	// cancellation, closing the stream to release the blocked read.
	readWithContext bool
	// wrap annotates a failed step. A nil wrap returns step errors unchanged.
	wrap func(guestControlStep, error) error
}

// exchange performs one request/response operation on a new guest stream. The
// stream is closed before exchange returns.
func (g guestControl) exchange(ctx context.Context, call guestControlExchange) error {
	fail := func(step guestControlStep, err error) error {
		if call.wrap == nil {
			return err
		}
		return call.wrap(step, err)
	}
	stream, err := g.machine.OpenStream(ctx)
	if err != nil {
		return fail(guestControlOpen, err)
	}
	defer stream.Close()
	switch call.closeOnCancel {
	case guestControlCloseOnCancelAsync:
		stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
		defer stop()
	case guestControlCloseOnCancelAwait:
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
		defer func() {
			if !stop() {
				<-closed
			}
		}()
	}
	if err := wire.WriteStreamFrameHeader(stream, call.header, 0); err != nil {
		return fail(guestControlWriteHeader, err)
	}
	if err := frameio.WriteProtoFrame(stream, call.request); err != nil {
		return fail(guestControlWriteRequest, err)
	}
	if call.readWithContext {
		err = readComputerControlResponse(ctx, stream, call.response)
	} else {
		err = frameio.ReadProtoFrame(stream, call.response)
	}
	if err != nil {
		return fail(guestControlReadResponse, err)
	}
	return nil
}

func readComputerControlResponse(ctx context.Context, stream vm.Stream, message proto.Message) error {
	result := make(chan error, 1)
	go func() { result <- frameio.ReadProtoFrame(stream, message) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = stream.Close()
		return ctx.Err()
	}
}
