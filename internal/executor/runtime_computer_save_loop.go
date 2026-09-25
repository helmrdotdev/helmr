package executor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Each execution lends its current authority to the physical mount. Waiting or
// finished executions return nil; unregistering does not change a pending save.
type computerSaveAuthority struct {
	current func() *workerapi.ComputerSaveBeginRequest
}

func (s *runtimeComputerSaves) attach(runtimeID, computerID string, current func() *workerapi.ComputerSaveBeginRequest) (func(), error) {
	if ids.Validate(runtimeID) != nil || ids.Validate(computerID) != nil || current == nil {
		return nil, errors.New("Computer save authority identity required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, errors.New("Computer saves are quiescing")
	}
	if s.runtimeID != "" && (s.runtimeID != runtimeID || s.computerID != computerID) {
		return nil, errors.New("Computer save authority belongs to another Runtime")
	}
	s.runtimeID, s.computerID = runtimeID, computerID
	authority := &computerSaveAuthority{current: current}
	s.authorities = append(s.authorities, authority)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, entry := range s.authorities {
				if entry == authority {
					s.authorities = append(s.authorities[:i], s.authorities[i+1:]...)
					break
				}
			}
		})
	}, nil
}

func (s *runtimeComputerSaves) authority() (*workerapi.ComputerSaveBeginRequest, string, string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, "", ""
	}
	authorities := append([]*computerSaveAuthority(nil), s.authorities...)
	runtimeID, computerID := s.runtimeID, s.computerID
	s.mu.Unlock()
	// Never call an execution owner while holding the mount owner lock.
	for _, authority := range authorities {
		if request := authority.current(); request != nil {
			copy := *request
			if request.Lease != nil {
				lease := *request.Lease
				copy.Lease = &lease
			}
			return &copy, runtimeID, computerID
		}
	}
	return nil, runtimeID, computerID
}

// run starts exactly one loop per physical Runtime. The interval is supplied by
// the Worker preservation policy; Turn completion and idleTimeout never tick it.
// The returned channel reports completion, including failure requiring source
// cleanup. Quiesce cancels and joins this loop before settling its pending save.
func (s *runtimeComputerSaves) run(ctx context.Context, interval time.Duration, client ComputerSaveClient, objects generationObjectPublisher, capture func(context.Context) (computerSaveCapture, error), onFailure func(error)) (<-chan error, error) {
	if interval <= 0 || client == nil || objects == nil || capture == nil || onFailure == nil {
		return nil, errors.New("Computer save loop dependencies and positive interval required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.loopDone != nil {
		return nil, errors.New("Computer save loop already started or stopped")
	}
	ctx, cancel := context.WithCancel(ctx)
	s.loopCancel = cancel
	s.loopDone = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(s.loopDone)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		err := s.loop(ctx, ticker.C, client, objects, capture)
		if err != nil && ctx.Err() == nil {
			onFailure(err)
		}
		result <- err
		close(result)
	}()
	return result, nil
}

func (s *runtimeComputerSaves) loop(ctx context.Context, ticks <-chan time.Time, client ComputerSaveClient, objects generationObjectPublisher, capture func(context.Context) (computerSaveCapture, error)) error {
	var observed *computerSave
	for {
		s.mu.Lock()
		pending := s.pending
		s.mu.Unlock()
		var completed <-chan struct{}
		if pending != nil && pending != observed {
			completed = pending.done
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-completed:
			settleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			if pending.err != nil {
				err = s.settle(settleCtx)
			}
			// Reclaim only after the exact published source has been adopted.
			// This work is outside the VM pause and publication critical path.
			if err == nil && pending.adopted && pending.capture != nil {
				_, err = pending.capture.Collect(settleCtx, 1<<20)
			}
			cancel()
			if err != nil {
				return err
			}
			observed = pending
		case _, open := <-ticks:
			if !open {
				return nil
			}
			// Completion, including settlement and maintenance, owns the next
			// transition even if a tick arrives at the same time.
			if pending != nil && pending != observed {
				continue
			}
			request, runtimeID, computerID := s.authority()
			if request == nil {
				continue
			}
			if _, err := s.start(ctx, client, objects, *request, runtimeID, computerID, capture); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}
