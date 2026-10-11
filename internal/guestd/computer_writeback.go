package guestd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

// One descriptor is retained before customer writes. Its error history must not
// be reset by reopening the filesystem for each save. The owner joins syncfs;
// closing an RPC does not cancel kernel writeback or create another flush owner.
type computerWriteback struct {
	mu              sync.Mutex
	syncFilesystem  func() error
	closeFilesystem func() error
	fault           error
	closed          bool
}

var errComputerWriteback = errors.New("computer writeback integrity fault")

func (w *computerWriteback) flush(ctx context.Context) error {
	if w == nil {
		return errors.New("computer writeback unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("computer writeback closed")
	}
	if w.fault != nil {
		return errComputerWriteback
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.syncFilesystem(); err != nil {
		w.fault = err
		return errComputerWriteback
	}
	return ctx.Err()
}

func (w *computerWriteback) close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.closeFilesystem()
}

func handleComputerFlush(ctx context.Context, conn programConnection, header wire.StreamHeader, bodyLen uint64, writeback *computerWriteback) error {
	if bodyLen != 0 {
		return errors.New("computer flush requires empty stream body")
	}
	if err := conn.SetReadDeadline(time.Now().Add(computerControlTimeout)); err != nil {
		return err
	}
	var request computerv0.FlushComputerRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return err
	}
	if ids.Validate(request.OperationId) != nil || request.OperationId != header.OperationID {
		return errors.New("computer flush operation identity required")
	}
	response := &computerv0.FlushComputerResponse{OperationId: request.OperationId}
	// This call remains synchronous even if its peer stops waiting. The host
	// must fence the source on uncertain writeback, never infer an adequate cut.
	if err := writeback.flush(ctx); err != nil {
		response.Error = "writeback_failed"
	}
	if err := conn.SetWriteDeadline(time.Now().Add(computerControlTimeout)); err != nil {
		return err
	}
	return frameio.WriteProtoFrame(conn, response)
}
