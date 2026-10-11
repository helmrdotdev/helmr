package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type ControlPlane interface {
	AuthenticateWorker(context.Context) error
	ReportWorkerStartupRecovery(context.Context, workerapi.StartupRecoveryRequest) error
	CompleteWorkerDrain(context.Context) (workerapi.StatusResponse, error)
	ActivateWorker(context.Context, workerapi.Capabilities) (workerapi.StatusResponse, error)
	ObserveWorker(context.Context, workerapi.Observation) (workerapi.StatusResponse, error)
}

type Work func(context.Context) error

type FatalWorkError interface {
	error
	FatalWorker() bool
}

type Consumer interface {
	Claim(context.Context, ConsumerAdmission) (Work, bool, error)
}

type ConsumerSpec struct {
	Name        string
	Concurrency int
	Admission   string
	// ContinueDuringDrain keeps the claim loop open after dispatch has been
	// durably closed. The consumer must only return work already bound to this
	// Worker.
	ContinueDuringDrain bool
	// BypassAdmissionDuringDrain is reserved for cleanup that cannot create or
	// start workload. Bound execution continuation must retain hard admission.
	BypassAdmissionDuringDrain bool
	Consumer                   Consumer
}

type BackgroundSpec struct {
	Name          string
	DrainEligible bool
	Run           func(context.Context) error
}

type Config struct {
	ControlPlane         ControlPlane
	Capabilities         workerapi.Capabilities
	Recover              func(context.Context) (RecoveryEvidence, error)
	FinalizeDrain        func(context.Context) (RecoveryEvidence, error)
	DrainCompleted       func(workerapi.StatusResponse) error
	Consumers            []ConsumerSpec
	Admission            map[string]int
	Background           []BackgroundSpec
	ObservationEvery     time.Duration
	PollEvery            time.Duration
	ProcessShutdownGrace time.Duration
	Observation          func(Status, Snapshot, RecoveryEvidence) workerapi.Observation
	AdmissionEvaluator   AdmissionEvaluator
	Log                  *slog.Logger
}

type Status string

const (
	StatusStarting Status = "starting"
	StatusActive   Status = "active"
	StatusDraining Status = "draining"
	StatusStopped  Status = "stopped"
)

type Snapshot struct {
	Active map[string]int
}

type Registry struct {
	mu     sync.Mutex
	active map[string]int
	wake   chan struct{}
}

func newRegistry() *Registry {
	return &Registry{active: map[string]int{}, wake: make(chan struct{}, 1)}
}

func (r *Registry) begin(kind string) func() {
	r.mu.Lock()
	r.active[kind]++
	r.mu.Unlock()
	r.notify()
	var once sync.Once
	return func() { once.Do(func() { r.mu.Lock(); r.active[kind]--; r.mu.Unlock(); r.notify() }) }
}

func (r *Registry) snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := make(map[string]int, len(r.active))
	maps.Copy(active, r.active)
	return Snapshot{Active: active}
}

func (r *Registry) empty() bool {
	for _, count := range r.snapshot().Active {
		if count != 0 {
			return false
		}
	}
	return true
}

func (r *Registry) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

type Supervisor struct {
	cfg       Config
	registry  *Registry
	admission map[string]chan struct{}
	state     atomic.Value
	recovery  RecoveryEvidence
}

func New(cfg Config) (*Supervisor, error) {
	if cfg.Recover == nil {
		return nil, errors.New("supervisor physical recovery is required")
	}
	if cfg.ControlPlane == nil {
		return nil, errors.New("supervisor control plane client is required")
	}
	if cfg.ObservationEvery <= 0 {
		cfg.ObservationEvery = workerapi.WorkerObservationInterval
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 2 * time.Second
	}
	if cfg.ProcessShutdownGrace <= 0 {
		cfg.ProcessShutdownGrace = 30 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	for _, spec := range cfg.Consumers {
		if spec.Name == "" || spec.Concurrency <= 0 || spec.Consumer == nil {
			return nil, errors.New("consumer name, positive concurrency, and implementation are required")
		}
		if spec.BypassAdmissionDuringDrain && !spec.ContinueDuringDrain {
			return nil, fmt.Errorf("consumer %s cannot bypass drain admission without continuing during drain", spec.Name)
		}
	}
	admission := make(map[string]chan struct{}, len(cfg.Admission))
	for name, capacity := range cfg.Admission {
		if name == "" || capacity <= 0 {
			return nil, errors.New("admission name and positive capacity are required")
		}
		admission[name] = make(chan struct{}, capacity)
	}
	for _, spec := range cfg.Consumers {
		if spec.Admission != "" && admission[spec.Admission] == nil {
			return nil, fmt.Errorf("consumer %s references unknown admission %s", spec.Name, spec.Admission)
		}
	}
	s := &Supervisor{cfg: cfg, registry: newRegistry(), admission: admission}
	s.state.Store(StatusStarting)
	return s, nil
}

func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.cfg.ControlPlane.AuthenticateWorker(ctx); err != nil {
		return fmt.Errorf("establish worker epoch: %w", err)
	}
	evidence, err := s.cfg.Recover(ctx)
	if err != nil {
		return fmt.Errorf("recover local worker state: %w", err)
	}
	for _, diagnostic := range evidence.QuarantineErrors {
		s.cfg.Log.WarnContext(ctx, "worker state quarantined", "error", diagnostic)
	}
	instanceQuarantines, err := activationQuarantines(evidence)
	if err != nil {
		return err
	}
	if err := s.reportStartupRecovery(ctx, workerapi.StartupRecoveryRequest{Quarantined: append([]string{}, evidence.Quarantined...)}); err != nil {
		return fmt.Errorf("record worker startup recovery: %w", err)
	}
	capabilities := s.cfg.Capabilities
	if instanceQuarantines != 0 {
		capabilities.ExecutionSlotsAvailable -= int32(instanceQuarantines)
		if capabilities.ExecutionSlotsAvailable <= 0 {
			return errors.New("all instance execution slots remain quarantined after startup recovery")
		}
	}
	status, err := s.cfg.ControlPlane.ActivateWorker(ctx, capabilities)
	if err != nil {
		return fmt.Errorf("activate worker: %w", err)
	}
	if status.Status != workerapi.StatusActive && status.Status != workerapi.StatusDraining {
		return fmt.Errorf("activated worker returned status %s", status.Status)
	}
	s.recovery = evidence
	if status.Status == workerapi.StatusActive {
		s.state.Store(StatusActive)
	} else {
		s.state.Store(StatusDraining)
	}
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	activeClaimCtx, cancelActiveClaims := context.WithCancel(workCtx)
	defer cancelActiveClaims()
	drainClaimCtx, cancelDrainClaims := context.WithCancel(workCtx)
	defer cancelDrainClaims()
	activeBackgroundCtx, cancelActiveBackground := context.WithCancel(workCtx)
	defer cancelActiveBackground()
	drainBackgroundCtx, cancelDrainBackground := context.WithCancel(workCtx)
	defer cancelDrainBackground()
	observeCtx, cancelObserve := context.WithCancel(workCtx)
	defer cancelObserve()
	var consumerWG sync.WaitGroup
	var backgroundWG sync.WaitGroup
	var observeWG sync.WaitGroup
	fatalWork := make(chan error, 1)
	for _, spec := range s.cfg.Consumers {
		claimCtx := activeClaimCtx
		if spec.ContinueDuringDrain {
			claimCtx = drainClaimCtx
		}
		for range spec.Concurrency {
			consumerWG.Go(func() { s.consume(claimCtx, workCtx, spec, evidence, fatalWork) })
		}
	}
	for _, background := range s.cfg.Background {
		backgroundCtx := activeBackgroundCtx
		if background.DrainEligible {
			backgroundCtx = drainBackgroundCtx
		}
		backgroundWG.Go(func() {
			if err := background.Run(backgroundCtx); err != nil && !errors.Is(err, context.Canceled) {
				var fatal FatalWorkError
				if errors.As(err, &fatal) && fatal.FatalWorker() {
					select {
					case fatalWork <- err:
					default:
					}
					return
				}
				s.cfg.Log.Error("worker background consumer stopped", "consumer", background.Name, "error", err)
			}
		})
	}
	drainRequested := make(chan struct{}, 1)
	var drainOnce sync.Once
	signalDrain := func(returned workerapi.StatusResponse) {
		if returned.Status == workerapi.StatusDraining {
			drainOnce.Do(func() {
				// Publish draining before waking Run so new-work claim loops close
				// while bound-work continuation sees the durable lifecycle state.
				s.state.Store(StatusDraining)
				drainRequested <- struct{}{}
			})
		}
	}
	observeWG.Go(func() { s.observe(observeCtx, evidence, signalDrain, fatalWork) })
	signalDrain(status)
	if status.Status == workerapi.StatusActive {
		select {
		case fatalErr := <-fatalWork:
			cancelActiveClaims()
			cancelDrainClaims()
			cancelActiveBackground()
			cancelDrainBackground()
			cancelObserve()
			cancelWork()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ProcessShutdownGrace)
			defer cancel()
			if !waitGroup(shutdownCtx, &consumerWG) ||
				!waitGroup(shutdownCtx, &backgroundWG) ||
				!waitGroup(shutdownCtx, &observeWG) {
				return fmt.Errorf("worker fatal execution did not stop all local work: %w", fatalErr)
			}
			s.state.Store(StatusStopped)
			return fmt.Errorf("worker fatal execution: %w", fatalErr)
		case <-ctx.Done():
			// A returned draining response stores StatusDraining before publishing
			// drainRequested. If shutdown and that response become ready together,
			// the durable latch wins over select's otherwise-random choice.
			if s.state.Load().(Status) != StatusDraining {
				return s.shutdownProcess(ctx, cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork, &consumerWG, &backgroundWG, &observeWG)
			}
		case <-drainRequested:
		}
	}
	return s.completeServerDirectedDrain(ctx, cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork, &consumerWG, &backgroundWG, &observeWG, evidence, fatalWork)
}

func (s *Supervisor) reportStartupRecovery(
	ctx context.Context,
	request workerapi.StartupRecoveryRequest,
) error {
	const maxAttempts = 10
	delay := s.cfg.PollEvery
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := s.cfg.ControlPlane.ReportWorkerStartupRecovery(ctx, request)
		if err == nil {
			return nil
		}
		var statusErr interface{ HTTPStatusCode() int }
		if !errors.As(err, &statusErr) || statusErr.HTTPStatusCode() != http.StatusConflict {
			return err
		}
		if attempt == maxAttempts {
			return fmt.Errorf("startup recovery conflict did not clear after %d attempts: %w", maxAttempts, err)
		}
		s.cfg.Log.Info("worker startup recovery is waiting for ownership reconciliation")
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay = min(delay*2, 30*time.Second)
		}
	}
	return errors.New("startup recovery retry exhausted")
}

func (s *Supervisor) shutdownProcess(
	ctx context.Context,
	cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork context.CancelFunc,
	consumerWG, backgroundWG, observeWG *sync.WaitGroup,
) error {
	s.state.Store(StatusDraining)
	cancelActiveClaims()
	cancelDrainClaims()
	cancelActiveBackground()
	cancelDrainBackground()
	// Ordinary process shutdown is deliberately non-durable. It waits committed
	// work but never submits a drain-completion proof, so systemd can restart the
	// worker and establish a fresh recovery epoch.
	drainCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ProcessShutdownGrace)
	defer cancel()
	if !waitGroup(drainCtx, consumerWG) {
		cancelWork()
		s.state.Store(StatusStopped)
		return fmt.Errorf("worker drain timed out: %w", ctx.Err())
	}
	cancelObserve()
	cancelWork()
	if !waitGroup(drainCtx, backgroundWG) || !waitGroup(drainCtx, observeWG) {
		s.state.Store(StatusStopped)
		return fmt.Errorf("worker shutdown timed out: %w", ctx.Err())
	}
	s.state.Store(StatusStopped)
	return ctx.Err()
}

func (s *Supervisor) completeServerDirectedDrain(
	ctx context.Context,
	cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork context.CancelFunc,
	consumerWG, backgroundWG, observeWG *sync.WaitGroup,
	startupEvidence RecoveryEvidence,
	fatalWork <-chan error,
) error {
	s.state.Store(StatusDraining)
	cancelActiveClaims()
	cancelActiveBackground()
	// Once the control plane has durably requested draining, process signals can stop
	// admission but cannot turn the operation back into a non-durable restart.
	// Planned drain has no destructive deadline: admitted work and renewals
	// continue until both physical and Control Plane authority are empty.
	drainCtx := context.WithoutCancel(ctx)
	fail := func(err error) error {
		cancelDrainClaims()
		cancelDrainBackground()
		cancelObserve()
		cancelWork()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ProcessShutdownGrace)
		defer cancel()
		if !waitGroup(shutdownCtx, consumerWG) || !waitGroup(shutdownCtx, backgroundWG) || !waitGroup(shutdownCtx, observeWG) {
			err = errors.Join(err, errors.New("worker drain failure did not stop all local work before process shutdown"))
		}
		s.state.Store(StatusStopped)
		return err
	}
	checkFatalWork := func() error {
		select {
		case err := <-fatalWork:
			return fmt.Errorf("worker fatal execution during drain: %w", err)
		default:
			return nil
		}
	}
	if err := checkFatalWork(); err != nil {
		return fail(err)
	}
	if err := s.waitForDrainReady(drainCtx, startupEvidence, fatalWork); err != nil {
		return fail(err)
	}
	if err := checkFatalWork(); err != nil {
		return fail(err)
	}
	// Freeze cleanup admission before final proof. Claim consumers finish before
	// background reconcilers, then the finalizer gets exclusive ownership of
	// local instance/process/netns cleanup.
	cancelDrainClaims()
	consumerWG.Wait()
	if err := checkFatalWork(); err != nil {
		return fail(err)
	}
	cancelDrainBackground()
	backgroundWG.Wait()
	if err := s.waitForDrainReady(drainCtx, startupEvidence, fatalWork); err != nil {
		return fail(err)
	}
	cancelObserve()
	observeWG.Wait()
	if s.cfg.FinalizeDrain == nil {
		return fail(errors.New("worker durable drain finalizer is required"))
	}
	finalEvidence, err := s.cfg.FinalizeDrain(drainCtx)
	if err != nil {
		return fail(fmt.Errorf("finalize local worker drain: %w", err))
	}
	if finalEvidence.ObservedAt.IsZero() || len(finalEvidence.Reclaimed) != 0 || len(finalEvidence.Quarantined) != 0 || len(finalEvidence.QuarantineErrors) != 0 {
		return fail(fmt.Errorf("final worker drain inventory is not clean: reclaimed=%d quarantined=%d errors=%d", len(finalEvidence.Reclaimed), len(finalEvidence.Quarantined), len(finalEvidence.QuarantineErrors)))
	}
	status, err := s.cfg.ControlPlane.CompleteWorkerDrain(drainCtx)
	if err != nil {
		return fail(fmt.Errorf("complete worker drain with clean local inventory: %w", err))
	}
	if status.Status != workerapi.StatusTerminationReady {
		return fail(fmt.Errorf("complete worker drain returned status %s, want termination_ready", status.Status))
	}
	if s.cfg.DrainCompleted != nil {
		if err := s.cfg.DrainCompleted(status); err != nil {
			s.cfg.Log.Warn("persist local drain completion marker", "error", err)
		}
	}
	cancelWork()
	s.state.Store(StatusStopped)
	return nil
}

func activationQuarantines(evidence RecoveryEvidence) (int, error) {
	if len(evidence.Quarantined) != len(evidence.QuarantinedOwners) {
		return 0, errors.New("worker activation is blocked by residue without exact VM ownership")
	}
	instanceCount := 0
	for _, owner := range evidence.QuarantinedOwners {
		if owner.Kind != vm.OwnerInstance {
			return 0, errors.New("worker activation is blocked by unknown VM owner kind")
		}
		instanceCount++
	}
	return instanceCount, nil
}

// Only credential-issuance rejection proves lost host authority. An ordinary
// request can race another claim change even after the client's refresh retry.
func workerAuthorityRejected(err error) bool {
	var rejected interface{ WorkerAuthorityRejected() bool }
	return errors.As(err, &rejected) && rejected.WorkerAuthorityRejected()
}

func (s *Supervisor) waitForDrainReady(ctx context.Context, evidence RecoveryEvidence, fatalWork <-chan error) error {
	ticker := time.NewTicker(s.cfg.PollEvery)
	defer ticker.Stop()
	for {
		if s.registry.empty() {
			status, err := s.observeOnce(ctx, s.observation(ctx, StatusDraining, evidence))
			if err == nil && status.Status == workerapi.StatusDraining && status.ActiveInstances == 0 {
				return nil
			}
			if workerAuthorityRejected(err) {
				return fmt.Errorf("worker drain authority rejected: %w", err)
			}
			if err != nil {
				s.cfg.Log.Warn("worker drain status observation failed", "error", err)
			}
		}
		select {
		case err := <-fatalWork:
			return fmt.Errorf("worker fatal execution during drain: %w", err)
		case <-s.registry.wake:
		case <-ticker.C:
		}
	}
}

func waitGroup(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Supervisor) consume(
	claimCtx context.Context,
	workCtx context.Context,
	spec ConsumerSpec,
	evidence RecoveryEvidence,
	fatalWork chan<- error,
) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-claimCtx.Done():
			return
		case <-timer.C:
		}
		state := s.state.Load().(Status)
		if state != StatusActive && !(spec.ContinueDuringDrain && state == StatusDraining) {
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		releaseAdmission, ok := s.acquireAdmission(claimCtx, spec.Admission)
		if !ok {
			return
		}
		state = s.state.Load().(Status)
		if state != StatusActive && !(spec.ContinueDuringDrain && state == StatusDraining) {
			releaseAdmission()
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		work, ok, err := spec.Consumer.Claim(claimCtx, consumerAdmission{supervisor: s, spec: spec, recovery: evidence})
		if err != nil {
			releaseAdmission()
			if claimCtx.Err() != nil {
				return
			}
			if errors.Is(err, errClaimAdmissionPaused) {
				timer.Reset(s.cfg.PollEvery)
				continue
			}
			s.cfg.Log.Error("worker claim failed", "consumer", spec.Name, "error", err)
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		if !ok {
			releaseAdmission()
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		if work == nil {
			releaseAdmission()
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		finish := s.registry.begin(spec.Name)
		retryDelay := time.Duration(0)
		if err := work(workCtx); err != nil && !errors.Is(err, context.Canceled) {
			var fatal FatalWorkError
			if errors.As(err, &fatal) && fatal.FatalWorker() {
				select {
				case fatalWork <- err:
				default:
				}
				finish()
				releaseAdmission()
				return
			}
			s.cfg.Log.Error("worker execution failed", "consumer", spec.Name, "error", err)
			retryDelay = s.cfg.PollEvery
		}
		finish()
		releaseAdmission()
		timer.Reset(retryDelay)
	}
}

func (s *Supervisor) acquireAdmission(ctx context.Context, name string) (func(), bool) {
	if name == "" {
		return func() {}, true
	}
	sem := s.admission[name]
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, true
	case <-ctx.Done():
		return nil, false
	}
}

// A black-holed request must not prevent fresh observations or drain completion
// after recovery. One observation period leaves room for retries before staleness.
func (s *Supervisor) observeOnce(ctx context.Context, observation workerapi.Observation) (workerapi.StatusResponse, error) {
	observationCtx, cancel := context.WithTimeout(ctx, s.cfg.ObservationEvery)
	defer cancel()
	return s.cfg.ControlPlane.ObserveWorker(observationCtx, observation)
}

func (s *Supervisor) observe(ctx context.Context, evidence RecoveryEvidence, statusReturned func(workerapi.StatusResponse), fatalWork chan<- error) {
	ticker := time.NewTicker(s.cfg.ObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		state := s.state.Load().(Status)
		status, err := s.observeOnce(ctx, s.observation(ctx, state, evidence))
		if err != nil && ctx.Err() == nil {
			if workerAuthorityRejected(err) {
				select {
				case fatalWork <- fmt.Errorf("worker observation authority rejected: %w", err):
				default:
				}
				return
			}
			s.cfg.Log.Warn("worker observation failed", "error", err)
		} else if err == nil {
			statusReturned(status)
		}
	}
}

func (s *Supervisor) AdmitInstanceStart(ctx context.Context) error {
	return s.admitInstanceStart(ctx, false)
}

func (s *Supervisor) admitInstanceStart(ctx context.Context, allocated bool) error {
	if s.cfg.AdmissionEvaluator == nil {
		return nil
	}
	decision := s.cfg.AdmissionEvaluator.Evaluate(ctx, AdmissionCheck{
		Consumer: "instance", Status: s.state.Load().(Status), Snapshot: s.registry.snapshot(), Recovery: s.recovery, DrainContinuation: allocated,
	})
	if !decision.Allowed {
		return fmt.Errorf("instance start admission paused: %s", decision.Reason)
	}
	return nil
}

func (s *Supervisor) observation(ctx context.Context, state Status, evidence RecoveryEvidence) workerapi.Observation {
	if s.cfg.Observation != nil {
		return s.cfg.Observation(state, s.registry.snapshot(), evidence)
	}
	observation := workerapi.Observation{}
	if s.cfg.AdmissionEvaluator != nil {
		// Health recovery must be observable even with no delivered owners: CP
		// placement and first delivery may themselves be paused by this report.
		healthCtx, cancel := context.WithTimeout(ctx, s.cfg.ObservationEvery)
		s.cfg.AdmissionEvaluator.Evaluate(healthCtx, AdmissionCheck{Consumer: "instance", Status: state, DrainContinuation: true})
		cancel()
		admissionObservation := s.cfg.AdmissionEvaluator.Observation()
		observation.RunPausedReason = admissionObservation.RunPausedReason
		observation.VMPausedReason = admissionObservation.VMPausedReason
	}
	if len(evidence.Quarantined) > 0 {
		observation.RunPausedReason = "startup_recovery_leak"
		observation.VMPausedReason = "startup_recovery_leak"
		return observation
	}
	if state != StatusActive && state != StatusDraining {
		observation.RunPausedReason = string(state)
		observation.VMPausedReason = string(state)
	}
	return observation
}
