package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Append acknowledgement precedes reading the next event, including the terminal
// result. A transport reconnect replays stable guest chunks; admission deduplicates
// them before Command completion closes the producer fence.
func (m ComputerMaterializer) readCommandOutput(ctx context.Context, reader io.ReadCloser, mount workerapi.ComputerInstanceAssignment, command workerapi.ComputerCommand, client workerapi.ComputerMaterializerControlPlaneClient) (*computerv0.ComputerBasicExecResult, error) {
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = reader.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	next := map[string]uint64{"stdout": 0, "stderr": 0}
	for {
		var event computerv0.ComputerBasicExecEvent
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := frameio.ReadProtoFrameBounded(reader, (192<<10)+4096, &event); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read command output: %w", err)
		}
		switch value := event.GetEvent().(type) {
		case *computerv0.ComputerBasicExecEvent_Output:
			chunk := value.Output
			sequence, valid := next[chunk.GetStream()]
			if !valid || chunk.GetSequence() != sequence || sequence > math.MaxInt64 || chunk.GetObservedAtUnixNano() <= 0 || len(chunk.GetContent()) == 0 || len(chunk.GetContent()) > 192<<10 {
				return nil, computerBasicExecProtocol(errors.New("invalid command output chunk"))
			}
			request := workerapi.CommandLogAppendRequest{OrgID: mount.OrgID, CommandID: command.CommandID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStream(chunk.GetStream()), ObservedSeq: chunk.GetSequence(), ObservedAt: time.Unix(0, chunk.GetObservedAtUnixNano()).UTC(), Content: chunk.GetContent()}
			if err := m.appendCommandOutput(ctx, client, request); err != nil {
				return nil, err
			}
			next[chunk.GetStream()]++
		case *computerv0.ComputerBasicExecEvent_Result:
			if value.Result == nil {
				return nil, computerBasicExecProtocol(errors.New("empty command result"))
			}
			return value.Result, nil
		default:
			return nil, computerBasicExecProtocol(errors.New("unknown command output event"))
		}
	}
}

func (m ComputerMaterializer) appendCommandOutput(ctx context.Context, client workerapi.ComputerMaterializerControlPlaneClient, request workerapi.CommandLogAppendRequest) error {
	backoff := m.CompleteErrorBackoff
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := client.AppendCommandLog(ctx, request)
		if err == nil {
			return nil
		}
		if !computerCommandCompletionRetryable(err) {
			return computerBasicExecProtocol(fmt.Errorf("append command output rejected: %w", err))
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}
