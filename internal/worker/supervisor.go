package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type Control interface {
	AuthenticateWorker(context.Context) error
	ReportWorkerStartupRecovery(context.Context, api.WorkerStartupRecoveryRequest) error
	CompleteWorkerDrain(context.Context, api.WorkerDrainCompletionRequest) (api.WorkerStatusResponse, error)
	ActivateWorker(context.Context, api.WorkerCapabilities) (api.WorkerStatusResponse, error)
	ObserveWorker(context.Context, api.WorkerObservation) (api.WorkerStatusResponse, error)
	RenewWorkerCertification(context.Context, api.WorkerCapabilities) (api.WorkerStatusResponse, error)
}

type Work func(context.Context) error

type Consumer interface {
	Claim(context.Context) (Work, bool, error)
}

type ConsumerSpec struct {
	Name        string
	Concurrency int
	Admission   string
	// DrainEligible permits cleanup work to continue while a durable,
	// server-directed drain is in progress. Execution and build consumers must
	// leave this false.
	DrainEligible bool
	Consumer      Consumer
}

type BackgroundSpec struct {
	Name          string
	DrainEligible bool
	Run           func(context.Context) error
}

type Config struct {
	Control            Control
	Capabilities       api.WorkerCapabilities
	Recover            func(context.Context) (RecoveryEvidence, error)
	FinalizeDrain      func(context.Context) (RecoveryEvidence, error)
	DrainCompleted     func(api.WorkerStatusResponse) error
	Consumers          []ConsumerSpec
	Admission          map[string]int
	Background         []BackgroundSpec
	ObservationEvery   time.Duration
	CertificationTTL   time.Duration
	PollEvery          time.Duration
	DrainTimeout       time.Duration
	Observation        func(State, Snapshot, RecoveryEvidence) api.WorkerObservation
	AdmissionEvaluator AdmissionEvaluator
	Log                *slog.Logger
}

type State string

const (
	StateStarting State = "starting"
	StateActive   State = "active"
	StateDraining State = "draining"
	StateStopped  State = "stopped"
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
	cfg                   Config
	registry              *Registry
	admission             map[string]chan struct{}
	state                 atomic.Value
	certifiedAt           atomic.Value
	recovery              RecoveryEvidence
	certifiedCapabilities api.WorkerCapabilities
}

func New(cfg Config) (*Supervisor, error) {
	if cfg.Control == nil {
		return nil, errors.New("supervisor control client is required")
	}
	if cfg.ObservationEvery <= 0 {
		cfg.ObservationEvery = 30 * time.Second
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 2 * time.Second
	}
	if cfg.CertificationTTL <= 0 {
		cfg.CertificationTTL = 24 * time.Hour
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 30 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	for _, spec := range cfg.Consumers {
		if spec.Name == "" || spec.Concurrency <= 0 || spec.Consumer == nil {
			return nil, errors.New("consumer name, positive concurrency, and implementation are required")
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
	s.state.Store(StateStarting)
	return s, nil
}

func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.cfg.Control.AuthenticateWorker(ctx); err != nil {
		return fmt.Errorf("establish worker epoch: %w", err)
	}
	evidence := RecoveryEvidence{ObservedAt: time.Now().UTC()}
	if s.cfg.Recover != nil {
		var err error
		evidence, err = s.cfg.Recover(ctx)
		if err != nil {
			return fmt.Errorf("recover local worker state: %w", err)
		}
	}
	inventory := append(append(make([]string, 0, len(evidence.Reclaimed)+len(evidence.Quarantined)), evidence.Reclaimed...), evidence.Quarantined...)
	if err := s.cfg.Control.ReportWorkerStartupRecovery(ctx, api.WorkerStartupRecoveryRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        evidence.ObservedAt,
		Inventory:         inventory,
		Reclaimed:         evidence.Reclaimed,
		Quarantined:       evidence.Quarantined,
		Errors:            evidence.QuarantineErrors,
	}); err != nil {
		return fmt.Errorf("record worker startup recovery: %w", err)
	}
	capabilities := s.cfg.Capabilities
	if capabilities.SupportsRun && len(evidence.Quarantined) != 0 {
		capabilities.ExecutionSlotsAvailable -= int32(len(evidence.Quarantined))
		if capabilities.MaxRuntimeStarts > capabilities.ExecutionSlotsAvailable {
			capabilities.MaxRuntimeStarts = capabilities.ExecutionSlotsAvailable
		}
		if capabilities.ExecutionSlotsAvailable <= 0 {
			return errors.New("all runtime execution slots remain quarantined after startup recovery")
		}
	}
	capabilities.Observation = s.observation(StateStarting, evidence)
	s.certifiedCapabilities = capabilities
	status, err := s.cfg.Control.ActivateWorker(ctx, capabilities)
	if err != nil {
		return fmt.Errorf("activate worker: %w", err)
	}
	if status.Status != api.WorkerStatusActive && status.Status != api.WorkerStatusDraining {
		return fmt.Errorf("activated worker returned status %s", status.Status)
	}
	s.certifiedAt.Store(time.Now().UTC())
	s.recovery = evidence
	if status.Status == api.WorkerStatusActive {
		s.state.Store(StateActive)
	} else {
		s.state.Store(StateDraining)
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
	for _, spec := range s.cfg.Consumers {
		claimCtx := activeClaimCtx
		if spec.DrainEligible {
			claimCtx = drainClaimCtx
		}
		for range spec.Concurrency {
			consumerWG.Go(func() { s.consume(claimCtx, workCtx, spec, evidence) })
		}
	}
	for _, background := range s.cfg.Background {
		backgroundCtx := activeBackgroundCtx
		if background.DrainEligible {
			backgroundCtx = drainBackgroundCtx
		}
		backgroundWG.Go(func() {
			if err := background.Run(backgroundCtx); err != nil && !errors.Is(err, context.Canceled) {
				s.cfg.Log.Error("worker background consumer stopped", "consumer", background.Name, "error", err)
			}
		})
	}
	drainRequested := make(chan struct{}, 1)
	var drainOnce sync.Once
	signalDrain := func(returned api.WorkerStatusResponse) {
		if returned.Status == api.WorkerStatusDraining {
			drainOnce.Do(func() {
				// Publish draining before waking Run so every claim loop closes
				// admission at the same instant the server response is observed.
				s.state.Store(StateDraining)
				drainRequested <- struct{}{}
			})
		}
	}
	observeWG.Go(func() { s.observe(observeCtx, evidence, signalDrain) })
	signalDrain(status)
	if status.Status == api.WorkerStatusActive {
		select {
		case <-ctx.Done():
			// A returned draining response stores StateDraining before publishing
			// drainRequested. If shutdown and that response become ready together,
			// the durable latch wins over select's otherwise-random choice.
			if s.state.Load().(State) != StateDraining {
				return s.shutdownProcess(ctx, cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork, &consumerWG, &backgroundWG, &observeWG)
			}
		case <-drainRequested:
		}
	}
	return s.completeServerDirectedDrain(ctx, cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork, &consumerWG, &backgroundWG, &observeWG, evidence)
}

func (s *Supervisor) shutdownProcess(
	ctx context.Context,
	cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork context.CancelFunc,
	consumerWG, backgroundWG, observeWG *sync.WaitGroup,
) error {
	s.state.Store(StateDraining)
	cancelActiveClaims()
	cancelDrainClaims()
	cancelActiveBackground()
	cancelDrainBackground()
	// Ordinary process shutdown is deliberately non-durable. It waits committed
	// work but never submits a drain-completion proof, so systemd can restart the
	// worker and establish a fresh recovery epoch.
	drainCtx, cancel := context.WithTimeout(context.Background(), s.cfg.DrainTimeout)
	defer cancel()
	if !waitGroup(drainCtx, consumerWG) {
		cancelWork()
		s.state.Store(StateStopped)
		return fmt.Errorf("worker drain timed out: %w", ctx.Err())
	}
	cancelObserve()
	cancelWork()
	if !waitGroup(drainCtx, backgroundWG) || !waitGroup(drainCtx, observeWG) {
		s.state.Store(StateStopped)
		return fmt.Errorf("worker shutdown timed out: %w", ctx.Err())
	}
	s.state.Store(StateStopped)
	return ctx.Err()
}

func (s *Supervisor) completeServerDirectedDrain(
	ctx context.Context,
	cancelActiveClaims, cancelDrainClaims, cancelActiveBackground, cancelDrainBackground, cancelObserve, cancelWork context.CancelFunc,
	consumerWG, backgroundWG, observeWG *sync.WaitGroup,
	startupEvidence RecoveryEvidence,
) error {
	s.state.Store(StateDraining)
	cancelActiveClaims()
	cancelActiveBackground()
	// Once control has durably requested draining, process signals can stop
	// admission but cannot turn the operation back into a non-durable restart.
	// The supervisor owns this bounded completion context.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.DrainTimeout)
	defer cancel()
	fail := func(err error) error {
		cancelDrainClaims()
		cancelDrainBackground()
		cancelObserve()
		cancelWork()
		s.state.Store(StateStopped)
		return err
	}
	if err := s.waitForDrainReady(drainCtx, startupEvidence); err != nil {
		return fail(err)
	}
	// Freeze cleanup admission before final proof. Claim consumers finish before
	// background reconcilers, then the finalizer gets exclusive ownership of
	// local runtime/process/netns cleanup.
	cancelDrainClaims()
	if !waitGroup(drainCtx, consumerWG) {
		return fail(fmt.Errorf("worker durable drain timed out waiting for cleanup claims: %w", drainCtx.Err()))
	}
	cancelDrainBackground()
	if !waitGroup(drainCtx, backgroundWG) {
		return fail(fmt.Errorf("worker durable drain timed out waiting for cleanup services: %w", drainCtx.Err()))
	}
	if err := s.waitForDrainReady(drainCtx, startupEvidence); err != nil {
		return fail(err)
	}
	cancelObserve()
	if !waitGroup(drainCtx, observeWG) {
		return fail(fmt.Errorf("worker durable drain timed out stopping observations: %w", drainCtx.Err()))
	}
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
	request := api.WorkerDrainCompletionRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        finalEvidence.ObservedAt,
		Inventory:         []string{},
		Reclaimed:         finalEvidence.Reclaimed,
		Quarantined:       finalEvidence.Quarantined,
		Errors:            finalEvidence.QuarantineErrors,
	}
	status, err := s.cfg.Control.CompleteWorkerDrain(drainCtx, request)
	if err != nil {
		return fail(fmt.Errorf("complete worker drain with clean local inventory: %w", err))
	}
	if status.Status != api.WorkerStatusDisabled {
		return fail(fmt.Errorf("complete worker drain returned status %s, want disabled", status.Status))
	}
	if s.cfg.DrainCompleted != nil {
		if err := s.cfg.DrainCompleted(status); err != nil {
			s.cfg.Log.Warn("persist local drain completion marker", "error", err)
		}
	}
	cancelWork()
	s.state.Store(StateStopped)
	return nil
}

func (s *Supervisor) waitForDrainReady(ctx context.Context, evidence RecoveryEvidence) error {
	ticker := time.NewTicker(s.cfg.PollEvery)
	defer ticker.Stop()
	for {
		if s.registry.empty() {
			status, err := s.cfg.Control.ObserveWorker(ctx, s.observation(StateDraining, evidence))
			if err == nil && status.Status == api.WorkerStatusDraining && status.ActiveExecutions == 0 {
				return nil
			}
			if err != nil && ctx.Err() == nil {
				s.cfg.Log.Warn("worker drain status observation failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker durable drain timed out before local and server authority reached zero: %w", ctx.Err())
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

func (s *Supervisor) consume(claimCtx context.Context, workCtx context.Context, spec ConsumerSpec, evidence RecoveryEvidence) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-claimCtx.Done():
			return
		case <-timer.C:
		}
		state := s.state.Load().(State)
		if state != StateActive && !(spec.DrainEligible && state == StateDraining) {
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		releaseAdmission, ok := s.acquireAdmission(claimCtx, spec.Admission)
		if !ok {
			return
		}
		state = s.state.Load().(State)
		if state != StateActive && !(spec.DrainEligible && state == StateDraining) {
			releaseAdmission()
			timer.Reset(s.cfg.PollEvery)
			continue
		}
		// Drain-eligible consumers perform cleanup rather than create workload.
		// Host admission (for example, low disk or unavailable KVM) must not
		// deadlock that cleanup once the server has durably closed placement.
		if s.cfg.AdmissionEvaluator != nil && !(spec.DrainEligible && state == StateDraining) {
			certifiedAt, _ := s.certifiedAt.Load().(time.Time)
			decision := s.cfg.AdmissionEvaluator.Evaluate(claimCtx, AdmissionCheck{
				Consumer: spec.Name, State: s.state.Load().(State), Snapshot: s.registry.snapshot(),
				Recovery: evidence, CertifiedAt: certifiedAt, CertificationTTL: s.cfg.CertificationTTL,
			})
			if !decision.Allowed {
				releaseAdmission()
				timer.Reset(s.cfg.PollEvery)
				continue
			}
		}
		work, ok, err := spec.Consumer.Claim(claimCtx)
		if err != nil {
			releaseAdmission()
			if claimCtx.Err() != nil {
				return
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
		finish := s.registry.begin(spec.Name)
		if err := work(workCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.cfg.Log.Error("worker execution failed", "consumer", spec.Name, "error", err)
		}
		finish()
		releaseAdmission()
		timer.Reset(0)
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

func (s *Supervisor) observe(ctx context.Context, evidence RecoveryEvidence, statusReturned func(api.WorkerStatusResponse)) {
	interval := min(s.cfg.ObservationEvery, s.cfg.CertificationTTL/2)
	if interval <= 0 {
		interval = s.cfg.CertificationTTL
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		state := s.state.Load().(State)
		certifiedAt, _ := s.certifiedAt.Load().(time.Time)
		if state == StateActive && time.Now().After(certifiedAt.Add(s.cfg.CertificationTTL/2)) {
			capabilities := s.certifiedCapabilities
			capabilities.Observation = s.observation(state, evidence)
			if status, err := s.cfg.Control.RenewWorkerCertification(ctx, capabilities); err != nil {
				s.cfg.Log.Warn("worker certification renewal failed", "error", err)
			} else {
				statusReturned(status)
				if status.Status == api.WorkerStatusActive {
					s.certifiedAt.Store(time.Now().UTC())
				}
			}
		}
		if status, err := s.cfg.Control.ObserveWorker(ctx, s.observation(state, evidence)); err != nil && ctx.Err() == nil {
			s.cfg.Log.Warn("worker observation failed", "error", err)
		} else if err == nil {
			statusReturned(status)
		}
	}
}

func (s *Supervisor) AdmitRuntimeStart(ctx context.Context) error {
	if s.cfg.AdmissionEvaluator == nil {
		return nil
	}
	certifiedAt, _ := s.certifiedAt.Load().(time.Time)
	decision := s.cfg.AdmissionEvaluator.Evaluate(ctx, AdmissionCheck{
		Consumer: "runtime", State: s.state.Load().(State), Snapshot: s.registry.snapshot(), Recovery: s.recovery,
		CertifiedAt: certifiedAt, CertificationTTL: s.cfg.CertificationTTL,
	})
	if !decision.Allowed {
		return fmt.Errorf("runtime start admission paused: %s", decision.Reason)
	}
	return nil
}

func (s *Supervisor) observation(state State, evidence RecoveryEvidence) api.WorkerObservation {
	if s.cfg.Observation != nil {
		return s.cfg.Observation(state, s.registry.snapshot(), evidence)
	}
	observation := api.WorkerObservation{HealthDetails: evidence.HealthDetails(), LeakedSlotCount: int32(len(evidence.Quarantined))}
	if s.cfg.AdmissionEvaluator != nil {
		admissionObservation := s.cfg.AdmissionEvaluator.Observation()
		observation.CPUPressureBPS = admissionObservation.CPUPressureBPS
		observation.MemoryPressureBPS = admissionObservation.MemoryPressureBPS
		observation.WorkloadDiskPressureBPS = admissionObservation.WorkloadDiskPressureBPS
		observation.ScratchPressureBPS = admissionObservation.ScratchPressureBPS
		if len(admissionObservation.HealthDetails) != 0 {
			combined, err := json.Marshal(map[string]json.RawMessage{
				"startup_recovery": evidence.HealthDetails(),
				"hard_admission":   admissionObservation.HealthDetails,
			})
			if err == nil {
				observation.HealthDetails = combined
			}
		}
		if admissionObservation.RunPausedReason != "" {
			observation.RunPausedReason = admissionObservation.RunPausedReason
			observation.BuildPausedReason = admissionObservation.BuildPausedReason
			observation.RuntimePausedReason = admissionObservation.RuntimePausedReason
		}
	}
	if observation.LeakedSlotCount > 0 {
		observation.RunPausedReason = "startup_recovery_leak"
		observation.BuildPausedReason = "startup_recovery_leak"
		observation.RuntimePausedReason = "startup_recovery_leak"
		return observation
	}
	if state != StateActive {
		observation.RunPausedReason = string(state)
		observation.BuildPausedReason = string(state)
		observation.RuntimePausedReason = string(state)
	}
	return observation
}
