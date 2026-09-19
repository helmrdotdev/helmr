package executor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

// programProtocol keeps one reader across hot waits and physical pause handoffs.
// It stops before a stream frame so the existing capture owner can consume that
// frame and its body without a competing reader or lost buffered bytes.
type programProtocol struct {
	stream    vm.Stream
	reader    *bufio.Reader
	events    chan programRead
	pending   *programRead
	resume    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	writeMu   sync.Mutex
}
type programRead struct {
	event    *programv0.RunEvent
	physical bool
	err      error
}

func newProgramProtocol(stream vm.Stream) *programProtocol {
	p := &programProtocol{stream: stream, reader: bufio.NewReader(stream), events: make(chan programRead, 1), resume: make(chan struct{}), done: make(chan struct{})}
	go p.read()
	return p
}
func (p *programProtocol) read() {
	for {
		prefix, err := p.reader.Peek(4)
		if err != nil {
			p.deliver(programRead{err: err})
			return
		}
		if frameio.IsStreamFramePrefix(prefix) {
			if !p.deliver(programRead{physical: true}) {
				return
			}
			select {
			case <-p.resume:
				continue
			case <-p.done:
				return
			}
		}
		event := new(programv0.RunEvent)
		err = frameio.ReadProtoFrameBounded(p.reader, maxFreshOutcomeFrameBytes, event)
		if !p.deliver(programRead{event: event, err: err}) || err != nil {
			return
		}
	}
}
func (p *programProtocol) deliver(value programRead) bool {
	select {
	case p.events <- value:
		return true
	case <-p.done:
		return false
	}
}
func (p *programProtocol) Read(b []byte) (int, error) { return p.reader.Read(b) }
func (p *programProtocol) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.stream.Write(b)
}
func (p *programProtocol) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	return p.stream.Close()
}
func (p *programProtocol) next(ctx context.Context) (programRead, error) {
	if p.pending != nil {
		value := *p.pending
		p.pending = nil
		return value, value.err
	}
	select {
	case value := <-p.events:
		return value, value.err
	case <-ctx.Done():
		return programRead{}, ctx.Err()
	case <-p.done:
		return programRead{}, io.ErrClosedPipe
	}
}
func (p *programProtocol) takePhysical(ctx context.Context, handle func(context.Context, *programv0.RunEvent) error) error {
	for {
		r, err := p.next(ctx)
		if err != nil {
			return err
		}
		if r.physical {
			return nil
		}
		if err := handle(ctx, r.event); err != nil {
			return err
		}
	}
}
func (p *programProtocol) releasePhysical() {
	select {
	case p.resume <- struct{}{}:
	case <-p.done:
	}
}
func (program *freshProgram) readEvent(ctx context.Context, event *programv0.RunEvent) error {
	if program.protocol == nil {
		return readProtoFrameBoundedContext(ctx, program.session, maxFreshOutcomeFrameBytes, event)
	}
	r, err := program.protocol.next(ctx)
	if err != nil {
		return err
	}
	if r.physical {
		return errors.New("unexpected physical program frame")
	}
	proto.Reset(event)
	proto.Merge(event, r.event)
	return nil
}
func (program *freshProgram) controlStream() io.ReadWriteCloser {
	if program.protocol != nil {
		return program.protocol
	}
	return program.session.Stream()
}
func (task *guestRunLeaseTask) programStream() io.ReadWriteCloser {
	return task.program.controlStream()
}

type checkpointCall struct {
	ctx     context.Context
	request CheckpointRequest
	result  chan checkpointReply
}
type checkpointReply struct {
	value CheckpointResult
	err   error
}
type hotWaitCheckpointer struct {
	Checkpointer
	requests chan checkpointCall
}

func (c hotWaitCheckpointer) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (CheckpointResult, error) {
	call := checkpointCall{ctx: ctx, request: request, result: make(chan checkpointReply, 1)}
	select {
	case c.requests <- call:
	case <-ctx.Done():
		return CheckpointResult{}, ctx.Err()
	}
	select {
	case r := <-call.result:
		return r.value, r.err
	case <-ctx.Done():
		return CheckpointResult{}, ctx.Err()
	}
}

// runHotWait lets bounded non-consuming operations proceed while the durable
// wait is polled. Checkpoint work returns to this same reader owner before pause.
func (task *guestRunLeaseTask) runHotWait(ctx context.Context, request WaitRequest, run func(context.Context, WaitRequest) error) error {
	if task.program.protocol == nil {
		return run(ctx, request)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	calls := make(chan checkpointCall)
	if request.Checkpointer != nil {
		request.Checkpointer = hotWaitCheckpointer{Checkpointer: request.Checkpointer, requests: calls}
	}
	resuming := make(chan struct{})
	var resumeOnce sync.Once
	if resume := request.Resume; resume != nil {
		request.Resume = func(ctx context.Context, decision WaitResumeDecision) error {
			resumeOnce.Do(func() { close(resuming) })
			return resume(ctx, decision)
		}
	}
	done := make(chan error, 1)
	go func() { done <- run(waitCtx, request) }()
	awaitDone := func() error {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for {
		select {
		case err := <-done:
			return err
		case <-resuming:
			return awaitDone()
		case call := <-calls:
			result, err := task.checkpointer.CreateCheckpoint(call.ctx, call.request)
			call.result <- checkpointReply{value: result, err: err}
			// A checkpoint detaches this source. Its physical stream remains owned by
			// the checkpointer until the Wait owner has recorded ready or failure.
			return awaitDone()
		case r := <-task.program.protocol.events:
			select {
			case <-resuming:
				task.program.protocol.pending = &r
				return awaitDone()
			default:
			}
			if r.err != nil {
				return r.err
			}
			if r.physical {
				return errors.New("unsolicited physical frame during hot wait")
			}
			if err := task.processCheckpointRunEvent(ctx, r.event); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-task.program.protocol.done:
			return io.ErrClosedPipe
		}
	}
}
