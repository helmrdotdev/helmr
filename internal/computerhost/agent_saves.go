package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"log/slog"
	"os"
	"sync"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	autosaveQuiet             = 250 * time.Millisecond
	autosaveMaxWait           = time.Second
	saveDiscoveryCatchupEvery = autosaveMaxWait
)

type AgentSaveClient interface {
	ReportAgentStoragePressure(context.Context, workerapi.AgentStoragePressure) error
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
	client              AgentSaveClient
	objects             versionObjectPublisher
	identity            workerapi.AllocationIdentity
	machine             liveCaptureMachine
	pending             *workerapi.AgentSavePublication
	capture             computerSaveCapture
	captured, uploaded  bool
	requested           *workerapi.AgentSave
	discovered          *workerapi.AgentSavePending
	idle                func(context.Context) error
	cancelIdle          func()
	changes             func() disk.VersionChanges
	wake                <-chan struct{}
	savedGeneration     uint64
	retryDelay          time.Duration
	pinCommitted        func(context.Context) (disk.RetainedVersion, error)
	pressureMu          sync.Mutex
	pressureGate        sync.Mutex
	pressureSave        *workerapi.AgentSave
	pressureRequest     chan struct{}
	maintenance         *saveMaintenance
	retirement          *workerapi.AgentSaveRetirement
	retirementCandidate *saveMaintenance
	accountingDue       time.Time
	admissionRetryAt    time.Time
	phase               string
}

func (s *agentComputerSaves) run(ctx context.Context) error {
	if s.client == nil || s.objects == nil || s.machine == nil || s.changes == nil || s.pinCommitted == nil {
		return errors.New("computer saves require an owned machine, dirty observer and publication dependencies")
	}
	ctx, stop := context.WithCancelCause(ctx)
	pressureDone := make(chan error, 1)
	s.pressureRequest = make(chan struct{}, 1)
	go func() {
		err := s.runPressure(ctx)
		if err != nil && ctx.Err() == nil {
			stop(err)
		}
		pressureDone <- err
	}()
	defer func() { stop(context.Canceled); <-pressureDone }()
	discover := true
	nextDiscovery := time.Now().Add(saveDiscoveryCatchupEvery)
	attempt := 0
	for ctx.Err() == nil {
		if !time.Now().Before(nextDiscovery) {
			discover = true
		}
		changes := s.changes()
		dueAt := s.nextSaveTime(changes, time.Now())
		due := !dueAt.IsZero() && !time.Now().Before(dueAt)
		if discover || due || s.pending != nil || s.requested != nil {
			discover = false
			attempt++
			started := time.Now()
			previousGeneration := s.savedGeneration
			previousMaintenance := s.maintenance
			err := s.step(ctx)
			nextDiscovery = time.Now().Add(saveDiscoveryCatchupEvery)
			if errors.Is(err, errSavePreempted) {
				discover = true
				continue
			}
			if err != nil {
				phase, save := s.failurePhase(), ""
				if s.pending != nil {
					save = s.pending.Save.SaveID
				} else if s.requested != nil {
					save = s.requested.SaveID
				}
				retry := saveOperationRetryable(err)
				slog.Info("Computer save step failed", "computer_id", s.identity.OwnerID, "save_id", save, "phase", phase, "attempt", attempt, "retry", retry, "step_duration_ms", float64(time.Since(started))/float64(time.Millisecond), "error", err)
				if !retry {
					return err
				}
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				s.retryDelay = min(10*time.Second, max(100*time.Millisecond, s.retryDelay*2))
				timer := time.NewTimer(s.retryDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return context.Cause(ctx)
				case <-s.wake:
					timer.Stop()
					discover = true
				case <-timer.C:
				}
				continue
			}
			attempt, s.retryDelay = 0, 0
			if s.savedGeneration != previousGeneration || (previousMaintenance != nil && s.maintenance == nil) {
				discover = true
				continue
			}
		}
		if s.maintenance != nil {
			discover = true
			continue
		}
		state := s.changes()
		// Completion proof is a checkpoint prerequisite. Once it is settled,
		// the coherent checkpoint can preserve dirty bytes without waiting for
		// an optional live cut; admission and guest qualification still decide.
		if s.pending == nil && s.requested == nil && s.accountingDue.IsZero() && state.PressureSince.IsZero() && s.idle != nil {
			if err := s.idle(ctx); err != nil {
				return err
			}
		} else if s.cancelIdle != nil {
			s.cancelIdle()
		}
		// Checkpoint admission retains its one-second visit. A separate bounded
		// pending-save catch-up repairs missed hints without optional admission or
		// device I/O while clean.
		changes = s.changes()
		delay := s.saveWait(time.Now(), nextDiscovery, changes)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-s.wake:
			timer.Stop()
			discover = true
		case <-changes.Wake:
			timer.Stop()
		case <-timer.C:
		}
	}
	return context.Cause(ctx)
}

func (s *agentComputerSaves) failurePhase() string {
	if s.phase != "" {
		return s.phase
	}
	return "discover"
}

// Autosave deadlines follow mutation intervals, not Turn count or the end of
// the previous upload. A missing live-writer interval is conservatively due.
func (s *agentComputerSaves) nextSaveTime(state disk.VersionChanges, now time.Time) time.Time {
	var due time.Time
	dirty := state.Generation > s.savedGeneration
	if dirty {
		if state.ChangedAt.IsZero() || state.LastChangedAt.IsZero() {
			due = now
		} else {
			due = earlierTime(state.ChangedAt.Add(autosaveMaxWait), state.LastChangedAt.Add(autosaveQuiet))
		}
	}
	due = earlierTime(due, s.accountingDue)
	// A published clean cut already has the authority needed for adoption and
	// collection. Let that maintenance free space before admitting another cut.
	if !state.PressureSince.IsZero() && (dirty || s.maintenance == nil) {
		due = now
	}
	if !due.IsZero() {
		due = maxTime(due, s.admissionRetryAt)
	}
	return due
}

func (s *agentComputerSaves) saveWait(now, discovery time.Time, state disk.VersionChanges) time.Duration {
	deadline := earlierTime(discovery, s.nextSaveTime(state, now))
	return max(time.Millisecond, deadline.Sub(now))
}

func earlierTime(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (s *agentComputerSaves) discover(ctx context.Context, retirement *workerapi.AgentSaveRetirement) (workerapi.AgentSavePending, error) {
	started := time.Now()
	state := s.changes()
	dueAt := s.nextSaveTime(state, started)
	admit := !dueAt.IsZero() && !started.Before(dueAt)
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	next, err := s.client.NextAgentSave(callCtx, workerapi.AgentSaveDiscovery{
		AllocationIdentity: s.identity, Admit: admit,
		BackgroundDue: admit && (state.Generation > s.savedGeneration || !state.PressureSince.IsZero()), Retirement: retirement,
	})
	if err != nil {
		return next, err
	}
	if next.PendingForMillis != nil {
		if next.Save != nil || *next.PendingForMillis < 0 {
			return next, &saveProtocolError{errors.New("invalid pending save metadata")}
		}
		// Age is measured at the durable source; anchor to this call's local
		// monotonic start rather than comparing clocks across machines. Ages
		// beyond the maximum already mean due and need no larger subtraction.
		since := started.Add(-time.Duration(min(*next.PendingForMillis, autosaveMaxWait.Milliseconds())) * time.Millisecond)
		wait := autosaveQuiet
		if state.Generation > s.savedGeneration {
			wait = autosaveMaxWait
		}
		// Later observations can shorten an established proof deadline, never
		// extend it when new mutations become visible.
		s.accountingDue = earlierTime(s.accountingDue, since.Add(wait))
	} else {
		s.accountingDue = time.Time{}
	}
	if admit && next.Save == nil && next.PendingForMillis == nil {
		// An optional request may be refused during checkpoint/owner transition.
		s.admissionRetryAt = time.Now().Add(time.Second)
	}
	return next, nil
}

func (s *agentComputerSaves) step(ctx context.Context) error {
	if s.cancelIdle != nil && (s.pending != nil || s.requested != nil || s.maintenance != nil) {
		s.cancelIdle()
	}
	if s.pending == nil {
		if s.requested == nil {
			s.phase = "discover"
			var next workerapi.AgentSavePending
			var err error
			if s.discovered != nil {
				next = *s.discovered
				s.discovered = nil
			} else {
				s.prepareRetirement()
				next, err = s.discover(ctx, s.retirement)
				if err == nil {
					s.retirement = nil
				}
			}
			if err != nil {
				return err
			}
			if s.cancelIdle != nil && (next.Save != nil || !s.accountingDue.IsZero() || s.maintenance != nil) {
				s.cancelIdle()
			}
			if next.Save == nil {
				return s.maintain(ctx)
			}
			if next.Sequence <= 0 || next.Save.EnvironmentID != s.identity.EnvironmentID || next.Save.LeaseEpoch != s.identity.Epoch {
				return &saveProtocolError{errors.New("save request differs from physical allocation")}
			}
			if _, err := uuid.Parse(next.Save.SaveID); err != nil {
				return &saveProtocolError{errors.New("invalid Save request identity")}
			}
			s.admissionRetryAt = time.Time{}
			s.requested = next.Save
			s.setPressureSave(next.Save)
		}
		// Retry the same admitted prefix until a cut exists. No root/evidence has
		// reached the server yet, so a repeated online flush may cover newer writes.
		started := time.Now()
		s.phase = "capture"
		capture, err := captureComputerSave(ctx, s.machine, s.identity.OwnerID)
		var generation uint64
		if capture != nil {
			generation = capture.Changes().Generation
		}
		if err != nil {
			s.phaseFinished(s.requested.SaveID, generation, "capture", started, err)
			return err
		}
		// Remote backing must finish while the server still accepts registrations
		// for this requested Save. It never owns this actual coverage receipt.
		s.pressureGate.Lock()
		s.setPressureSave(nil)
		s.pressureGate.Unlock()
		s.capture = capture
		s.pending = &workerapi.AgentSavePublication{Save: *s.requested, Root: capture.Root(), Evidence: uuid.NewV7().String()}
		s.phaseFinished(s.requested.SaveID, generation, "capture", started, nil)
	}
	if !s.captured {
		started := time.Now()
		s.phase = "record_capture"
		err := s.client.CaptureAgentSave(ctx, *s.pending)
		s.phaseFinished(s.pending.Save.SaveID, s.capture.Changes().Generation, "record_capture", started, err)
		if err != nil {
			return err
		}
		s.captured = true
	}
	if !s.uploaded {
		started := time.Now()
		s.phase = "publish_objects"
		err := s.capture.Publish(ctx, agentSavePublisher{client: s.client, objects: s.objects, save: s.pending.Save})
		s.phaseFinished(s.pending.Save.SaveID, s.capture.Changes().Generation, "publish_objects", started, err)
		if err != nil {
			return err
		}
		s.uploaded = true
	}
	started := time.Now()
	s.phase = "publish_save"
	err := s.client.PublishAgentSave(ctx, *s.pending)
	attributes := []any{}
	if first := s.capture.Changes().ChangedAt; !first.IsZero() {
		attributes = append(attributes, "oldest_change_age_ms", float64(time.Since(first))/float64(time.Millisecond))
	}
	s.phaseFinished(s.pending.Save.SaveID, s.capture.Changes().Generation, "publish_save", started, err, attributes...)
	if err != nil {
		return err
	}
	s.savedGeneration = s.capture.Changes().Generation
	// Prior maintenance has returned and joined its consumers. A newer published
	// cut supersedes that local work; releasing its pin does not retire remote
	// custody or advance the adopted head. Keep existing retirement receipts.
	if s.maintenance != nil {
		s.maintenance.capture.Release()
	}
	s.maintenance = &saveMaintenance{capture: s.capture, saveID: s.pending.Save.SaveID}
	s.capture, s.pending, s.requested = nil, nil, nil
	s.captured, s.uploaded = false, false
	return nil
}

var errSavePreempted = errors.New("new Computer save preempted local maintenance")

type saveMaintenance struct {
	capture computerSaveCapture
	saveID  string
	adopted bool
}

func (s *agentComputerSaves) maintain(ctx context.Context) error {
	if s.maintenance == nil {
		return nil
	}
	task := s.maintenance
	operationCtx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	var pending *workerapi.AgentSavePending
	var discoveryErr error
	go func() {
		defer close(joined)
		nextDiscovery := time.Now().Add(saveDiscoveryCatchupEvery)
		var retryAt time.Time
		for {
			state := s.changes()
			now := time.Now()
			delay := s.saveWait(now, nextDiscovery, state)
			if now.Before(retryAt) {
				delay = max(delay, time.Until(retryAt))
			}
			timer := time.NewTimer(delay)
			inspect := false
			select {
			case <-operationCtx.Done():
				timer.Stop()
				return
			case <-s.wake:
				inspect = true
			case <-state.Wake:
			case <-timer.C:
			}
			timer.Stop()
			now = time.Now()
			if now.Before(retryAt) {
				continue
			}
			dueAt := s.nextSaveTime(s.changes(), now)
			if !inspect && now.Before(nextDiscovery) && (dueAt.IsZero() || now.Before(dueAt)) {
				continue
			}
			// Metadata alone must not preempt maintenance or freeze coverage.
			next, err := s.discover(operationCtx, nil)
			nextDiscovery = time.Now().Add(saveDiscoveryCatchupEvery)
			if operationCtx.Err() != nil {
				return
			}
			if err != nil {
				if !saveOperationRetryable(err) {
					discoveryErr = err
					cancel()
					return
				}
				retryAt = time.Now().Add(time.Second)
				continue
			}
			retryAt = time.Time{}
			if next.Save != nil {
				pending = &next
				cancel()
				return
			}
		}
	}()
	var err error
	if !task.adopted {
		started := time.Now()
		s.phase = "adopt"
		err = task.capture.Adopt(operationCtx)
		s.phaseFinished(task.saveID, task.capture.Changes().Generation, "adopt", started, err)
		if err == nil {
			task.adopted = true
			s.retirementCandidate = task
		}
	}
	if err == nil {
		task.capture.Release()
		started := time.Now()
		s.phase = "collect"
		_, err = task.capture.Collect(operationCtx, 1<<20)
		s.phaseFinished(task.saveID, task.capture.Changes().Generation, "collect", started, err)
	}
	cancel()
	<-joined
	if err == nil {
		s.maintenance = nil
	}
	if discoveryErr != nil {
		return discoveryErr
	}
	if pending != nil {
		s.discovered = pending
		if ctx.Err() == nil && (err == nil || saveOperationRetryable(err)) {
			return errSavePreempted
		}
	}
	return err
}

func (s *agentComputerSaves) phaseFinished(saveID string, generation uint64, phase string, started time.Time, err error, attributes ...any) {
	if err != nil {
		return
	}
	fields := []any{"computer_id", s.identity.OwnerID, "lease_epoch", s.identity.Epoch, "save_id", saveID, "captured_generation", generation, "phase", phase, "duration_ms", float64(time.Since(started)) / float64(time.Millisecond)}
	slog.Info("Computer save phase completed", append(fields, attributes...)...)
}

// release is called after committed publication/local adoption or physical VM
// closure. It is never an inference that an uncertain remote write was absent.
func (s *agentComputerSaves) release() {
	if s.capture != nil {
		s.capture.Release()
	}
	if s.maintenance != nil {
		s.maintenance.capture.Release()
	}
	s.maintenance = nil
	s.retirementCandidate, s.retirement = nil, nil
	s.capture, s.pending, s.requested = nil, nil, nil
	s.captured, s.uploaded = false, false
}

type agentSavePublisher struct {
	client  AgentSaveClient
	objects versionObjectPublisher
	save    workerapi.AgentSave
}

func (p agentSavePublisher) RegisterBatch(ctx context.Context, inspections []blockformat.ObjectInspection) error {
	return p.client.RegisterAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspections: inspections})
}
func (p agentSavePublisher) CertifyBatch(ctx context.Context, inspections []blockformat.ObjectInspection) error {
	return p.client.CertifyAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspections: inspections})
}
func (p agentSavePublisher) Reuse(ctx context.Context, i blockformat.ObjectInspection) error {
	if err := p.RegisterBatch(ctx, []blockformat.ObjectInspection{i}); err != nil {
		return err
	}
	return p.CertifyBatch(ctx, []blockformat.ObjectInspection{i})
}
func (p agentSavePublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return p.objects.Publish(ctx, d, f)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// A lost discovery response retries the same exact floor. The candidate is
// process-local: reopening the durable head alone never announces a floor.
func (s *agentComputerSaves) prepareRetirement() {
	if task := s.retirementCandidate; task != nil && task.capture.ReadyForRetirement() {
		s.retirement = &workerapi.AgentSaveRetirement{SaveID: task.saveID, Root: task.capture.Root()}
		s.retirementCandidate = nil
	}
}
