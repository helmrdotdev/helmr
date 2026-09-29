package computerhost

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
// annotate its own failures.
type guestControlStep uint8

const (
	guestControlOpen guestControlStep = iota
	guestControlWriteHeader
	guestControlWriteRequest
	guestControlReadResponse
)

// guestControlCancellation is how context cancellation reaches an operation
// stream. Each operation keeps the cancellation behavior it has always had;
// the policies differ because the operations do, not by design of the helper.
type guestControlCancellation uint8

const (
	// guestControlCancelReadOnly leaves header and request writes unaffected
	// by cancellation; the response read returns ctx.Err() and closes the
	// stream. Used by authority renewal, program resume grant and restore
	// verification.
	guestControlCancelReadOnly guestControlCancellation = iota
	// guestControlCancelCloseStream closes the stream when ctx is done at any
	// step, without waiting for that close, and reads the response directly.
	// Used by freeze.
	guestControlCancelCloseStream
	// guestControlCancelCloseStreamAndRead closes the stream when ctx is done
	// at any step, without waiting for that close, and the response read
	// returns ctx.Err(). Used by restore installation and activation.
	guestControlCancelCloseStreamAndRead
	// guestControlCancelAwaitStreamClose closes the stream when ctx is done at
	// any step, reads the response directly and returns only after that close
	// has finished. Used by Command cancellation and Program run cleanup.
	guestControlCancelAwaitStreamClose
)

type guestControlExchange struct {
	header       wire.StreamHeader
	request      proto.Message
	response     proto.Message
	cancellation guestControlCancellation
}

// exchange performs one request/response operation on a new guest stream and
// closes the stream before returning. On failure it reports the failed step;
// the error itself is returned unchanged.
func (g guestControl) exchange(ctx context.Context, call guestControlExchange) (guestControlStep, error) {
	stream, err := g.machine.OpenStream(ctx)
	if err != nil {
		return guestControlOpen, err
	}
	defer stream.Close()
	switch call.cancellation {
	case guestControlCancelCloseStream, guestControlCancelCloseStreamAndRead:
		stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
		defer stop()
	case guestControlCancelAwaitStreamClose:
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
		defer func() {
			if !stop() {
				<-closed
			}
		}()
	}
	if step, err := writeGuestControlRequest(stream, call.header, call.request); err != nil {
		return step, err
	}
	switch call.cancellation {
	case guestControlCancelReadOnly, guestControlCancelCloseStreamAndRead:
		err = readResponseWithContext(ctx, stream, call.response)
	default:
		err = frameio.ReadProtoFrame(stream, call.response)
	}
	if err != nil {
		return guestControlReadResponse, err
	}
	return 0, nil
}

// writeGuestControlRequest writes one operation's stream header and request
// frame.
func writeGuestControlRequest(stream vm.Stream, header wire.StreamHeader, request proto.Message) (guestControlStep, error) {
	if err := wire.WriteStreamFrameHeader(stream, header, 0); err != nil {
		return guestControlWriteHeader, err
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return guestControlWriteRequest, err
	}
	return 0, nil
}

func readResponseWithContext(ctx context.Context, stream vm.Stream, message proto.Message) error {
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
