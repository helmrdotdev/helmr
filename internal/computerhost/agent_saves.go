package computerhost

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AgentSaveClient interface {
	NextAgentSave(context.Context, workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error)
	RegisterAgentSaveObject(context.Context, workerapi.AgentSaveObject) error
	CertifyAgentSaveObject(context.Context, workerapi.AgentSaveObject) error
	CaptureAgentSave(context.Context, workerapi.AgentSavePublication) error
	PublishAgentSave(context.Context, workerapi.AgentSavePublication) error
}

// agentComputerSaves is owned by one physical allocation. It processes durable
// Save requests serially, retaining the same local cut through uncertain replies.
// On failure the allocation must close the VM before releasing an unresolved cut.
type agentComputerSaves struct {
	client                                 AgentSaveClient
	objects                                versionObjectPublisher
	identity                               workerapi.AllocationIdentity
	machine                                liveCaptureMachine
	pending                                *workerapi.AgentSavePublication
	capture                                computerSaveCapture
	captured, uploaded, published, adopted bool
	captureErr                             error
	idle                                   func(context.Context) error
	every                                  time.Duration
	nextBackground                         time.Time
}

func (s *agentComputerSaves) run(ctx context.Context) error {
	if s.client == nil || s.objects == nil || s.machine == nil || s.every <= 0 {
		return errors.New("computer saves require an owned machine and publication dependencies")
	}
	attempt := 0
	for ctx.Err() == nil {
		attempt++
		started := time.Now()
		err := s.step(ctx)
		if err == nil && s.pending == nil && s.idle != nil {
			if err = s.idle(ctx); err != nil {
				return err
			}
		}
		if err != nil {
			phase, save := "discover", ""
			switch {
			case s.captureErr != nil:
				phase = "capture"
			case s.pending == nil:
			case !s.captured:
				phase = "record_capture"
			case !s.uploaded:
				phase = "publish_objects"
			case !s.published:
				phase = "publish_save"
			case !s.adopted:
				phase = "adopt"
			default:
				phase = "collect"
			}
			if s.pending != nil {
				save = s.pending.Save.SaveID
			}
			retry := s.captureErr == nil && (saveAdmissionRetryable(err) || errors.Is(err, cas.ErrUnavailable))
			slog.Info("Computer save step failed", "computer_id", s.identity.OwnerID, "save_id", save,
				"phase", phase, "attempt", attempt, "retry", retry, "step_duration_ms", float64(time.Since(started))/float64(time.Millisecond), "error", err)
			if !retry {
				return err
			}
		} else {
			attempt = 0
		}
		if err := sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (s *agentComputerSaves) step(ctx context.Context) error {
	if s.captureErr != nil {
		return s.captureErr
	}
	if s.pending == nil {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if s.nextBackground.IsZero() {
			s.nextBackground = time.Now().Add(s.every)
		}
		due := !time.Now().Before(s.nextBackground)
		next, err := s.client.NextAgentSave(callCtx, workerapi.AgentSaveDiscovery{AllocationIdentity: s.identity, BackgroundDue: due})
		cancel()
		if err != nil {
			return err
		}
		if next.Save == nil {
			if due {
				// A declined optional admission must not spin or delay idle work.
				s.nextBackground = time.Now().Add(time.Second)
			}
			return nil
		}
		if next.Sequence <= 0 || next.Save.EnvironmentID != s.identity.EnvironmentID || next.Save.LeaseEpoch != s.identity.Epoch {
			return errors.New("save request differs from physical allocation")
		}
		if _, err := uuid.Parse(next.Save.SaveID); err != nil {
			return errors.New("invalid Save request identity")
		}
		// Discovery committed before the live flush/capture. Never retry an ambiguous
		// physical cut: stop the allocation and retain the unresolved durable request.
		started := time.Now()
		capture, err := captureComputerSave(ctx, s.machine, s.identity.OwnerID)
		if err != nil {
			s.captureErr = err
			return err
		}
		s.capture = capture
		s.pending = &workerapi.AgentSavePublication{Save: *next.Save, Root: capture.Root(), Evidence: uuid.NewV7().String()}
		s.phaseCompleted("capture", started)
	}
	if !s.captured {
		started := time.Now()
		if err := s.client.CaptureAgentSave(ctx, *s.pending); err != nil {
			return err
		}
		s.captured = true
		s.phaseCompleted("record_capture", started)
	}
	if !s.uploaded {
		started := time.Now()
		if err := s.capture.Publish(ctx, agentSavePublisher{client: s.client, objects: s.objects, save: s.pending.Save}); err != nil {
			return err
		}
		s.uploaded = true
		s.phaseCompleted("publish_objects", started)
	}
	if !s.published {
		started := time.Now()
		if err := s.client.PublishAgentSave(ctx, *s.pending); err != nil {
			return err
		}
		s.published = true
		s.nextBackground = time.Now().Add(s.every)
		s.phaseCompleted("publish_save", started)
	}
	if !s.adopted {
		started := time.Now()
		if err := s.capture.Adopt(ctx, 1<<20); err != nil {
			return err
		}
		s.adopted = true
		s.phaseCompleted("adopt", started)
	}
	s.capture.Release()
	started := time.Now()
	if _, err := s.capture.Collect(ctx, 1<<20); err != nil {
		return err
	}
	s.phaseCompleted("collect", started)
	s.capture, s.pending = nil, nil
	s.captured, s.uploaded, s.published, s.adopted = false, false, false, false
	return nil
}

func (s *agentComputerSaves) phaseCompleted(phase string, started time.Time) {
	slog.Info("Computer save phase completed", "computer_id", s.identity.OwnerID, "save_id", s.pending.Save.SaveID,
		"phase", phase, "duration_ms", float64(time.Since(started))/float64(time.Millisecond))
}

// release is called after committed publication/local adoption or physical VM
// closure. It is never an inference that an uncertain remote write was absent.
func (s *agentComputerSaves) release() {
	if s.capture != nil {
		s.capture.Release()
	}
	s.capture, s.pending = nil, nil
	s.captured, s.uploaded, s.published, s.adopted = false, false, false, false
}

type agentSavePublisher struct {
	client  AgentSaveClient
	objects versionObjectPublisher
	save    workerapi.AgentSave
}

func (p agentSavePublisher) Register(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.RegisterAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspection: i})
}
func (p agentSavePublisher) Certify(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.CertifyAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspection: i})
}
func (p agentSavePublisher) Reuse(ctx context.Context, i blockformat.ObjectInspection) error {
	if err := p.Register(ctx, i); err != nil {
		return err
	}
	return p.Certify(ctx, i)
}
func (p agentSavePublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return p.objects.Publish(ctx, d, f)
}
