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

type guestControlExchange struct {
	header   wire.StreamHeader
	request  proto.Message
	response proto.Message
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
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	if step, err := writeGuestControlRequest(stream, call.header, call.request); err != nil {
		return step, err
	}
	err = frameio.ReadProtoFrame(stream, call.response)
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
