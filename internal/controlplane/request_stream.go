package controlplane

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Binary deployment bodies stay streaming. Their admitted descriptor and S3
// checksum classify invalid input at the upload handler, without JSON decoding.
// One progress watchdog covers client stalls, S3 backpressure and its final
// response. Cancellation interrupts an inbound body read, not only S3's request.
type deploymentUploadStream struct {
	readMu     sync.Mutex
	closed     bool
	ctx        context.Context
	cancel     context.CancelFunc
	body       io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration
	deadline   time.Time
	progress   atomic.Int64
	done       chan struct{}
}

func newDeploymentUploadStream(w http.ResponseWriter, r *http.Request, idle, maximum time.Duration) (*deploymentUploadStream, error) {
	ctx, cancel := context.WithTimeout(r.Context(), maximum)
	deadline, _ := ctx.Deadline()
	s := &deploymentUploadStream{ctx: ctx, cancel: cancel, body: r.Body,
		controller: http.NewResponseController(w), idle: idle, deadline: deadline, done: make(chan struct{})}
	s.progress.Store(time.Now().UnixNano())
	if err := s.setReadDeadline(); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(idle / 4)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Set the deadline before Close: HTTP/1 Close itself may wait for
				// an in-progress Read to release the body lock.
				_ = s.controller.SetReadDeadline(time.Now())
				s.readMu.Lock()
				s.closed = true
				_ = s.body.Close()
				s.readMu.Unlock()
				return
			case now := <-ticker.C:
				if now.Sub(time.Unix(0, s.progress.Load())) >= idle {
					cancel()
				}
			}
		}
	}()
	return s, nil
}

func (s *deploymentUploadStream) setReadDeadline() error {
	deadline := time.Now().Add(s.idle)
	if s.deadline.Before(deadline) {
		deadline = s.deadline
	}
	return s.controller.SetReadDeadline(deadline)
}

func (s *deploymentUploadStream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if err := s.setReadDeadline(); err != nil {
		return 0, err
	}
	n, err := s.body.Read(p)
	if n > 0 {
		s.progress.Store(time.Now().UnixNano())
	}
	return n, err
}

func (s *deploymentUploadStream) close() {
	s.cancel()
	<-s.done
}
