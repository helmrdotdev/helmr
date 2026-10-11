package computerhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Each pipe has at most one outstanding head. Independent append jobs leave the
// frame reader free to report terminal evidence while diagnostic admission waits.
func (m commandService) readCommandOutput(ctx context.Context, conn io.ReadWriteCloser, mount commandAuthority, command workerapi.ComputerCommand, client workerapi.ComputerCommandClient, onTerminal func(*computerv0.ComputerBasicExecResult) error) (*computerv0.ComputerBasicExecResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	defer func() { cancel(); _ = conn.Close(); jobs.Wait() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var mu, writeMu sync.Mutex
	pending := map[string]bool{}
	next := map[string]uint64{}
	ends := map[string]*computerv0.CommandOutputChunk{}
	var jobErr error
	fail := func(err error) {
		mu.Lock()
		if jobErr == nil {
			jobErr = err
		}
		mu.Unlock()
		cancel()
	}
	var result *computerv0.ComputerBasicExecResult
	for {
		var event computerv0.ComputerBasicExecEvent
		if err := frameio.ReadProtoFrameBounded(conn, agentTransportFrameLimit, &event); err != nil {
			mu.Lock()
			appendErr := jobErr
			stdout, stderr := ends["stdout"], ends["stderr"]
			unsettled := pending["stdout"] || pending["stderr"] ||
				(next["stdout"] != 0 && stdout == nil) || (next["stderr"] != 0 && stderr == nil)
			mu.Unlock()
			if appendErr != nil {
				return nil, appendErr
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) && result != nil {
				if unsettled {
					return nil, computerBasicExecProtocol(errors.New("command transport closed before observed output settled"))
				}
				if (stdout != nil && !commandBoundaryMatches(result.Stdout, stdout)) || (stderr != nil && !commandBoundaryMatches(result.Stderr, stderr)) {
					return nil, computerBasicExecProtocol(errors.New("command output end differs from terminal frontier"))
				}
				return result, nil
			}
			return nil, fmt.Errorf("read command output: %w", err)
		}
		switch value := event.GetEvent().(type) {
		case *computerv0.ComputerBasicExecEvent_Output:
			chunk := value.Output
			if chunk == nil || chunk.Sequence > math.MaxInt64 || chunk.ThroughSequence > math.MaxInt64 {
				return nil, computerBasicExecProtocol(errors.New("invalid command output range"))
			}
			record := diagnostic.Record{Stream: chunk.Stream, Kind: chunk.Kind, Sequence: int64(chunk.Sequence), ThroughSequence: int64(chunk.ThroughSequence), ObservedAtUnixNano: chunk.ObservedAtUnixNano, Data: chunk.Content, DroppedBytes: chunk.DroppedBytes, Complete: chunk.Complete}
			if record.Validate(agentTransportFrameLimit-64*1024) != nil {
				return nil, computerBasicExecProtocol(errors.New("invalid command diagnostic envelope"))
			}
			mu.Lock()
			invalid := pending[chunk.Stream] || ends[chunk.Stream] != nil || (next[chunk.Stream] != 0 && next[chunk.Stream] != chunk.Sequence)
			if !invalid {
				pending[chunk.Stream] = true
			}
			mu.Unlock()
			if invalid {
				return nil, computerBasicExecProtocol(errors.New("command pipe skipped or changed an outstanding head"))
			}
			if result != nil {
				boundary := result.Stdout
				if chunk.Stream == "stderr" {
					boundary = result.Stderr
				}
				if boundary == nil || chunk.ThroughSequence > boundary.ThroughSequence {
					return nil, computerBasicExecProtocol(errors.New("command output exceeds terminal frontier"))
				}
			}
			request := workerapi.CommandLogAppendRequest{EnvironmentID: mount.EnvironmentID, CommandID: command.CommandID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: command.WriterGeneration, Stream: workerapi.LogStream(chunk.Stream), ObservedSeq: chunk.Sequence, ThroughSequence: chunk.ThroughSequence, ObservedAt: time.Unix(0, chunk.ObservedAtUnixNano).UTC(), Content: chunk.Content, Kind: chunk.Kind, DroppedBytes: chunk.DroppedBytes, Complete: chunk.Complete}
			jobs.Go(func() {
				receipt, err := m.appendCommandOutput(ctx, client, request)
				if err != nil {
					fail(err)
					return
				}
				disposition := "accepted"
				if receipt.Expired {
					disposition = "expired"
				}
				// Record local progress before publishing the ACK: a fast guest may deliver
				// the successor as soon as the write completes.
				mu.Lock()
				pending[chunk.Stream] = false
				next[chunk.Stream] = chunk.ThroughSequence + 1
				if chunk.Kind == "end" {
					ends[chunk.Stream] = chunk
				}
				mu.Unlock()
				writeMu.Lock()
				err = frameio.WriteProtoFrame(conn, &computerv0.CommandOutputAck{Stream: chunk.Stream, ThroughSequence: chunk.ThroughSequence, Disposition: disposition})
				writeMu.Unlock()
				if err != nil {
					fail(err)
				}
			})
		case *computerv0.ComputerBasicExecEvent_Result:
			if value.Result == nil || result != nil {
				return nil, computerBasicExecProtocol(errors.New("empty or repeated command result"))
			}
			result = value.Result
			for _, boundary := range []*computerv0.CommandOutputBoundary{result.Stdout, result.Stderr} {
				if boundary == nil || boundary.ThroughSequence == 0 || boundary.ThroughSequence > math.MaxInt64 {
					return nil, computerBasicExecProtocol(errors.New("command result has no exact output boundary"))
				}
			}
			mu.Lock()
			invalid := next["stdout"] > result.Stdout.ThroughSequence+1 || next["stderr"] > result.Stderr.ThroughSequence+1
			for stream, end := range ends {
				boundary := result.Stdout
				if stream == "stderr" {
					boundary = result.Stderr
				}
				invalid = invalid || !commandBoundaryMatches(boundary, end)
			}
			mu.Unlock()
			if invalid {
				return nil, computerBasicExecProtocol(errors.New("command terminal frontier precedes observed output"))
			}
			if onTerminal != nil {
				if err := onTerminal(result); err != nil {
					return nil, err
				}
			}
		default:
			return nil, computerBasicExecProtocol(errors.New("unknown command output event"))
		}
	}
}
func commandBoundaryMatches(boundary *computerv0.CommandOutputBoundary, end *computerv0.CommandOutputChunk) bool {
	return boundary != nil && end != nil && boundary.ThroughSequence == end.ThroughSequence && boundary.Complete == end.Complete
}

func (m commandService) appendCommandOutput(ctx context.Context, client workerapi.ComputerCommandClient, request workerapi.CommandLogAppendRequest) (workerapi.DiagnosticLogReceipt, error) {
	backoff := m.CompleteErrorBackoff
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	var nextWarning time.Time
	for {
		if err := ctx.Err(); err != nil {
			return workerapi.DiagnosticLogReceipt{}, err
		}
		receipt, err := client.AppendCommandLog(ctx, request)
		if err == nil {
			if receipt.ThroughSequence != int64(request.ThroughSequence) || (receipt.Expired && (!receipt.AcceptedAt.IsZero() || !receipt.ExpiresAt.IsZero())) || (!receipt.Expired && (receipt.AcceptedAt.IsZero() || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour)) {
				return workerapi.DiagnosticLogReceipt{}, computerBasicExecProtocol(errors.New("invalid command diagnostic receipt"))
			}
			return receipt, nil
		}
		if !computerCommandCompletionRetryable(err) {
			return workerapi.DiagnosticLogReceipt{}, computerBasicExecProtocol(fmt.Errorf("append command output rejected: %w", err))
		}
		if now := time.Now(); ctx.Err() == nil && !now.Before(nextWarning) {
			status := 0
			var statusError interface{ HTTPStatusCode() int }
			if errors.As(err, &statusError) {
				status = statusError.HTTPStatusCode()
			}
			slog.WarnContext(ctx, "Computer command output append retry", "command_id", request.CommandID, "stream", request.Stream, "sequence", request.ObservedSeq, "http_status", status, "error", err)
			nextWarning = now.Add(5 * time.Second)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerapi.DiagnosticLogReceipt{}, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}
