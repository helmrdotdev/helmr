package executor

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/helmrdotdev/helmr/internal/httpclient"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
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
	computer.CapturedGeneration
	Adopt(context.Context, int) error
	Collect(context.Context, int) (int64, error)
}

// computerSave owns one fixed operation, including uncertain responses. Its
// Runtime owner serializes operations and supplies the monotonically increasing
// sequence. It must Quiesce before checkpoint/terminal capture or source release.
// A failed Quiesce does not authorize continuing the guest or dropping retention;
// the Runtime owner must retry within its deadline or use physical reclamation.
type computerSave struct {
	client     ComputerSaveClient
	objects    generationObjectPublisher
	request    workerapi.ComputerSaveBeginRequest
	runtimeID  string
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
// belongs to the Runtime stop path. The caller owns the lifetime of client,
// objects and capture dependencies until Quiesce has joined this operation.
func startComputerSave(ctx context.Context, client ComputerSaveClient, objects generationObjectPublisher, request workerapi.ComputerSaveBeginRequest, runtimeID, computerID string, capture func(context.Context) (computerSaveCapture, error)) (*computerSave, error) {
	if client == nil || objects == nil || capture == nil || ids.Validate(runtimeID) != nil || ids.Validate(computerID) != nil || ids.Validate(request.SaveID) != nil || request.Sequence <= 0 {
		return nil, errors.New("computer save dependencies and identities required")
	}
	// The request must remain stable even when the caller renews its lease value.
	if request.Lease != nil {
		lease := *request.Lease
		request.Lease = &lease
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &computerSave{client: client, objects: objects, request: request, runtimeID: runtimeID, computerID: computerID, cancel: cancel, done: make(chan struct{}), settle: make(chan struct{}, 1)}
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
	if response.RuntimeInstanceID != s.runtimeID || response.SaveID != s.request.SaveID || response.Sequence != s.request.Sequence || ids.Validate(response.WorkspaceLeaseID) != nil || ids.Validate(response.PredecessorID) != nil || response.DesiredVersion <= 0 {
		return errors.New("computer save admission differs from operation")
	}
	s.admitted = true
	return nil
}

func (s *computerSave) run(ctx context.Context, capture func(context.Context) (computerSaveCapture, error)) error {
	if err := s.begin(ctx); err != nil {
		var response *httpclient.Error
		if errors.As(err, &response) && response.StatusCode == http.StatusConflict {
			// A definitive rejection of this first request allocated no slot.
			// This does not apply to a replay after a lost admission response.
			s.rejected = true
			s.released = true
		}
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
	objects generationObjectPublisher
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
