package computerhost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// ComputerSaveClient is the host publication boundary. Repeated publication and
// adoption use the original request. Responses must never be inferred from the
// current Computer head, which may have advanced since the original operation.
type ComputerSaveClient interface {
	BeginComputerSave(context.Context, workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error)
	RegisterComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error
	CertifyComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error
	ReuseComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error
	PublishComputerSave(context.Context, workerapi.ComputerSavePublicationRequest) (workerapi.ComputerSavePublicationResponse, error)
	AdoptComputerSave(context.Context, workerapi.ComputerSavePublicationRequest) error
	AbandonComputerSave(context.Context, workerapi.ComputerSaveBeginRequest) error
}

type computerSaveCapture interface {
	disk.CapturedVersion
	Adopt(context.Context, int) error
	Collect(context.Context, int) (int64, error)
}

// computerSave owns one fixed operation, including uncertain responses. Its
// Instance owner serializes operations and supplies the monotonically increasing
// sequence. It must Quiesce before checkpoint/terminal capture or source release.
// A failed Quiesce does not authorize continuing the guest or dropping retention;
// the Instance owner must retry within its deadline or use physical reclamation.
type computerSave struct {
	client     ComputerSaveClient
	objects    versionObjectPublisher
	request    workerapi.ComputerSaveBeginRequest
	instanceID string
	computerID string
	capture    computerSaveCapture
	cancel     context.CancelFunc
	done       chan struct{}
	settle     chan struct{}
	err        error
	admitted   bool
	captureErr error
	committing bool
	committed  bool
	adopted    bool
	released   bool
	rejected   bool
}

// startComputerSave returns before remote work completes. Capture must return
// only after restoring normal guest/device dispatch; ambiguous capture failure
// belongs to the Instance stop path. The caller owns the lifetime of client,
// objects and capture dependencies until Quiesce has joined this operation.
func startComputerSave(ctx context.Context, client ComputerSaveClient, objects versionObjectPublisher, request workerapi.ComputerSaveBeginRequest, instanceID, computerID string, capture func(context.Context) (computerSaveCapture, error)) (*computerSave, error) {
	if client == nil || objects == nil || capture == nil || ids.Validate(instanceID) != nil || ids.Validate(computerID) != nil || ids.Validate(request.SaveID) != nil || request.Sequence <= 0 || ids.Validate(request.EnvironmentID) != nil || request.ComputerInstanceID != instanceID || request.WriterGeneration <= 0 {
		return nil, errors.New("computer save dependencies and identities required")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &computerSave{client: client, objects: objects, request: request, instanceID: instanceID, computerID: computerID, cancel: cancel, done: make(chan struct{}), settle: make(chan struct{}, 1)}
	s.settle <- struct{}{}
	go func() {
		defer close(s.done)
		defer cancel()
		s.err = s.run(ctx, capture)
	}()
	return s, nil
}

func (s *computerSave) begin(ctx context.Context) error {
	response, err := s.client.BeginComputerSave(ctx, s.request)
	if err != nil {
		return err
	}
	if response.ComputerInstanceID != s.instanceID || response.SaveID != s.request.SaveID || response.Sequence != s.request.Sequence || response.WriterGeneration != s.request.WriterGeneration || ids.Validate(response.PredecessorID) != nil || response.DesiredVersion <= 0 {
		return errors.New("computer save admission differs from operation")
	}
	s.admitted = true
	return nil
}

// awaitAdmission keeps an intact guest alive through temporary API failure. The
// owning writer's renewal context still ends this wait at its confirmed expiry.
// Every attempt uses the same slot identity; no capture starts before admission.
func (s *computerSave) awaitAdmission(ctx context.Context) error {
	delay := controlRequestRetryEvery
	started := time.Now()
	var lastWarning time.Time
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
		err := s.begin(callCtx)
		cancel()
		if err == nil {
			if attempt > 1 {
				slog.Info("computer save admission resumed", "computer_instance_id", s.instanceID, "save_id", s.request.SaveID)
			}
			return nil
		}
		if attempt == 1 && httpclient.IsStatus(err, http.StatusConflict) {
			// Only a definitive first rejection proves no slot was allocated.
			// A conflict after a lost reply retains the original pending slot.
			s.rejected, s.released = true, true
		}
		if !saveAdmissionRetryable(err) {
			return err
		}
		if lastWarning.IsZero() || time.Since(lastWarning) >= time.Minute {
			slog.Warn("computer save admission delayed", "computer_instance_id", s.instanceID, "save_id", s.request.SaveID,
				"elapsed_ms", time.Since(started).Milliseconds(), "attempts", attempt, "error", err)
			lastWarning = time.Now()
		}
		if err := sleepWithContext(ctx, delay); err != nil {
			return err
		}
		if delay < time.Second {
			delay = min(2*delay, time.Second)
		}
	}
}

func saveAdmissionRetryable(err error) bool {
	var response *httpclient.Error
	if errors.As(err, &response) {
		return response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
	}
	var transport net.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (s *computerSave) run(ctx context.Context, capture func(context.Context) (computerSaveCapture, error)) error {
	if err := s.awaitAdmission(ctx); err != nil {
		return err
	}
	var err error
	s.capture, err = capture(ctx)
	if err != nil || s.capture == nil {
		s.captureErr = errors.Join(errors.New("computer save capture did not establish a resumable cut"), err)
		return s.captureErr
	}
	if err = s.capture.Publish(ctx, computerSavePublisher{s.client, s.objects, s.request}); err != nil {
		return err
	}
	return s.finish(ctx)
}

func (s *computerSave) finish(ctx context.Context) error {
	request := workerapi.ComputerSavePublicationRequest{Save: s.request, Root: s.capture.Root()}
	if !s.committed {
		// Once sent, an error cannot prove absence of a committed Version. Join
		// the producer and replay this exact commit; never abandon based on EOF.
		s.committing = true
		response, err := s.client.PublishComputerSave(ctx, request)
		if err != nil {
			return err
		}
		if response.ComputerID != s.computerID || response.VersionID != s.request.SaveID {
			return errors.New("computer save receipt differs from operation")
		}
		s.committed = true
	}
	if !s.adopted {
		if err := s.capture.Adopt(ctx, 1<<20); err != nil {
			return err
		}
		s.adopted = true
	}
	if err := s.client.AdoptComputerSave(ctx, request); err != nil {
		return err
	}
	s.capture.Release()
	s.released = true
	return nil
}

// Wait reports the background attempt, including a possibly uncertain response.
// Quiesce must still settle an unsuccessful attempt before any subsequent cut.
func (s *computerSave) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.err
	}
}

// Quiesce stops and joins all producers before deciding whether to abandon or
// finish adoption. Its separate caller-supplied deadline bounds reconciliation.
// Repeated calls can finish a previous uncertain handoff, without recapturing or
// uploading different bytes. Concurrent callers serialize with cancellation.
func (s *computerSave) Quiesce(ctx context.Context) error {
	s.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.settle:
	}
	defer func() { s.settle <- struct{}{} }()
	if s.released {
		return nil
	}
	if s.captureErr != nil {
		return s.captureErr
	}
	if !s.admitted {
		// A lost admission reply may have reserved the slot. Reconcile before
		// abandonment, even if capture never started.
		if err := s.begin(ctx); err != nil {
			return err
		}
	}
	if s.committing {
		err := s.finish(ctx)
		var response *httpclient.Error
		if err == nil || s.committed || !errors.As(err, &response) || response.StatusCode != http.StatusConflict {
			return err
		}
		// A lifecycle transition may forbid a previously uncommitted publish.
		// Conflict is not absence: only an explicit atomic abandonment response
		// can release this cut. A racing committed receipt makes abandon fail.
		if abandonErr := s.abandon(ctx); abandonErr != nil {
			return errors.Join(err, abandonErr)
		}
		return nil
	}
	return s.abandon(ctx)
}

func (s *computerSave) abandon(ctx context.Context) error {
	if err := s.client.AbandonComputerSave(ctx, s.request); err != nil {
		return err
	}
	if s.capture != nil {
		s.capture.Release()
	}
	s.released = true
	return nil
}

type computerSavePublisher struct {
	client  ComputerSaveClient
	objects versionObjectPublisher
	save    workerapi.ComputerSaveBeginRequest
}

func (p computerSavePublisher) Register(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.RegisterComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.save, Inspection: i})
}
func (p computerSavePublisher) Certify(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.CertifyComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.save, Inspection: i})
}
func (p computerSavePublisher) Reuse(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.ReuseComputerSaveObject(ctx, workerapi.ComputerSaveObjectRequest{Save: p.save, Inspection: i})
}
func (p computerSavePublisher) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	return p.objects.Publish(ctx, d, file)
}
