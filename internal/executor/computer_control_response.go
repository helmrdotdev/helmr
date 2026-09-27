package executor

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

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
