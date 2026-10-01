package executor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/computerhost"
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
	streamMu  sync.Mutex
	paused    bool
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
			p.writeMu.Lock()
			p.paused = true
			p.writeMu.Unlock()
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
	p.streamMu.Lock()
	stream := p.stream
	p.streamMu.Unlock()
	return stream.Write(b)
}
func (p *programProtocol) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	return p.stream.Close()
}

// replacePausedStream transfers transport while the single reader is parked at
// the physical handoff. No goroutine may read the replacement before this send.
func (p *programProtocol) replacePausedStream(ctx context.Context, stream vm.Stream) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if !p.paused || p.reader.Buffered() != 0 {
		return errors.New("source protocol is not at an empty physical handoff")
	}
	p.streamMu.Lock()
	select {
	case <-p.done:
		p.streamMu.Unlock()
		return io.ErrClosedPipe
	default:
	}
	old := p.stream
	p.stream = stream
	p.reader = bufio.NewReader(stream)
	p.streamMu.Unlock()
	_ = old.Close()
	select {
	case p.resume <- struct{}{}:
		p.paused = false
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return io.ErrClosedPipe
	}
}

func (p *programProtocol) currentStream() vm.Stream {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	return p.stream
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

func (program *freshProgram) readEvent(ctx context.Context, event *programv0.RunEvent) error {
	if program.protocol == nil {
		return readProtoFrameBoundedContext(ctx, program.channel, maxFreshOutcomeFrameBytes, event)
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
	return program.channel.Stream()
}
func (task *guestRunLeaseTask) programStream() io.ReadWriteCloser {
	return task.program.controlStream()
}

// runHotWait lets bounded non-consuming operations proceed while the durable
// wait is polled. Physical capture is owned by the Computer coordinator.
var errCaptureResumed = errors.New("capture aborted and source resumed")

func (task *guestRunLeaseTask) runHotWait(ctx context.Context, request WaitRequest, run func(context.Context, WaitRequest) error) error {
	for {
		err := task.runHotWaitOnce(ctx, request, run)
		if !errors.Is(err, errCaptureResumed) {
			return err
		}
	}
}

func (task *guestRunLeaseTask) runHotWaitOnce(ctx context.Context, request WaitRequest, run func(context.Context, WaitRequest) error) (retErr error) {
	if task.program.protocol == nil {
		return run(ctx, request)
	}
	if task.captures == nil {
		return errors.New("computer capture registry is required for hot waits")
	}
	var captureRequests <-chan *computerhost.MemberPause
	{
		task.mu.Lock()
		lease := task.lease
		task.mu.Unlock()
		captureWait, err := task.captures.Register(lease, request.RunWaitID, task.resumeCapturedMember)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, captureWait.Detach()) }()
		captureRequests = captureWait.Pauses()
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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
		case pause := <-captureRequests:
			stopAbort := context.AfterFunc(ctx, func() { pause.Abort(ctx.Err()) })
			finish := func(err error) error {
				// Once a pause is dispatched the logical owner stays joined
				// through physical exclusion, even when its caller cancels.
				result := pause.Settle(err)
				stopAbort()
				if result != nil {
					return result
				}
				if pause.Resumed() {
					task.mu.Lock()
					task.capturePaused = false
					task.mu.Unlock()
					return errCaptureResumed
				}
				return ErrDetached
			}
			cancel()
			// Join the poller before transferring the reader. A simultaneous
			// resume decision invalidates this capture rather than being lost.
			pollErr := <-done
			select {
			case <-resuming:
				pause.Abort(errors.New("computer capture raced a wait resume"))
				if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
					return finish(pollErr)
				}
				result := finish(nil)
				if pollErr == nil && errors.Is(result, errCaptureResumed) {
					// ResumeDecision was already delivered before the queued
					// pause was joined. Do not poll or deliver the same wait again.
					return nil
				}
				return result
			default:
			}
			if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
				return finish(pollErr)
			}
			return finish(task.pauseComputerMember(pause.Context(), request, pause.Target(), pause.Member()))
		case err := <-done:
			return err
		case <-resuming:
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
