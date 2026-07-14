package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var errProviderMutationInProgress = errors.New("provider mutation in progress")

// GroupSnapshot is the authoritative control-plane view used for one
// reconciliation. ResourceIDs maps Helmr worker IDs to provider instance IDs;
// it is deliberately explicit so provider mutations can never accidentally use
// a logical worker ID.
type GroupSnapshot struct {
	Inputs               Inputs
	ResourceIDs          map[string]string
	OldestDrainStartedAt time.Time
}

// TerminationProof is re-read after planning and immediately before a provider
// termination. All three safety facts must be durable control-plane facts.
type TerminationProof struct {
	WorkerID             string
	ResourceID           string
	State                WorkerState
	AuthorityCount       uint64
	LocalCleanupComplete bool
	FencedForTermination bool
}

// ConfirmedAction is a provider effect that has been observed after a
// potentially ambiguous mutation response. Sources may keep these times in a
// process-local cooldown cache: restart can remove a delay, but cannot create
// capacity or satisfy termination proof.
type ConfirmedAction struct {
	Action     Action
	WorkerID   string
	ResourceID string
	Desired    int
	At         time.Time
}

// Every string scope argument is a worker-group ID.
type SnapshotSource interface {
	Snapshot(context.Context, string) (GroupSnapshot, error)
	MarkDraining(context.Context, string, string) error
	TerminationProof(context.Context, string, string) (TerminationProof, error)
	ClaimTermination(context.Context, string, string) (TerminationProof, error)
	RecordMutationIntent(context.Context, string, ConfirmedAction) error
	RecordConfirmed(context.Context, string, ConfirmedAction) error
}

// A controller that loses election performs no authoritative read or mutation.
type LeaderElector interface {
	TryAcquire(context.Context, string) (LeaderLease, bool, error)
}

type LeaderLease interface {
	Release() error
}

type ProviderInstance struct {
	ID                   string
	ProtectedFromScaleIn bool
	Lifecycle            string
}

type ProviderState struct {
	Desired   int
	Instances map[string]ProviderInstance
}

type Provider interface {
	Describe(context.Context, string) (ProviderState, error)
	SetDesired(context.Context, string, int) error
	SetScaleInProtection(context.Context, string, string, bool) error
	Terminate(context.Context, string, string, bool) error
}

type Clock interface {
	Now() time.Time
}

type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

type ControllerConfig struct {
	GroupID          string
	Interval         time.Duration
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
	MetricsTimeout   time.Duration
	OperationTimeout time.Duration
	DrainTimeout     time.Duration
}

type Controller struct {
	config             ControllerConfig
	planner            *Planner
	source             SnapshotSource
	leaders            LeaderElector
	provider           Provider
	metrics            MetricsPublisher
	clock              Clock
	sleeper            Sleeper
	underutilizedSince time.Time
}

var (
	ErrInvalidController    = errors.New("invalid fleet controller")
	ErrUnsafeTermination    = errors.New("unsafe fleet termination refused")
	ErrProviderNotConverged = errors.New("provider mutation not converged")
)

func NewController(config ControllerConfig, planner *Planner, source SnapshotSource, leaders LeaderElector, provider Provider, metrics MetricsPublisher, clock Clock, sleeper Sleeper) (*Controller, error) {
	if config.GroupID == "" || config.Interval <= 0 || config.InitialBackoff <= 0 || config.MaxBackoff < config.InitialBackoff || config.MetricsTimeout < 0 || config.OperationTimeout < 0 || config.DrainTimeout < 0 {
		return nil, fmt.Errorf("%w: invalid group or timing configuration", ErrInvalidController)
	}
	if config.MetricsTimeout == 0 {
		config.MetricsTimeout = 250 * time.Millisecond
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = 30 * time.Second
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = 30 * time.Minute
	}
	if config.OperationTimeout < config.MetricsTimeout {
		return nil, fmt.Errorf("%w: operation timeout must cover metrics timeout", ErrInvalidController)
	}
	if planner == nil || source == nil || leaders == nil || provider == nil {
		return nil, fmt.Errorf("%w: planner, source, leader elector, and provider are required", ErrInvalidController)
	}
	if clock == nil {
		clock = systemClock{}
	}
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	if metrics == nil {
		metrics = discardMetrics{}
	}
	return &Controller{
		config: config, planner: planner, source: source, leaders: leaders,
		provider: provider, metrics: metrics, clock: clock, sleeper: sleeper,
	}, nil
}

// Failures are isolated to this group and retried with bounded backoff.
func (c *Controller) Run(ctx context.Context) error {
	backoff := c.config.InitialBackoff
	for {
		cycleCtx, cancel := context.WithTimeout(ctx, c.config.OperationTimeout)
		err := c.Cycle(cycleCtx)
		cancel()
		delay := c.config.Interval
		if err != nil {
			delay = backoff
			backoff = doubledDurationBounded(backoff, c.config.MaxBackoff)
		} else {
			backoff = c.config.InitialBackoff
		}
		if err := c.sleeper.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// Cycle performs at most one planned lifecycle transition.
func (c *Controller) Cycle(ctx context.Context) (cycleErr error) {
	lease, acquired, err := c.leaders.TryAcquire(ctx, c.config.GroupID)
	if err != nil {
		return fmt.Errorf("acquire fleet leader lock for %q: %w", c.config.GroupID, err)
	}
	if !acquired {
		return nil
	}
	if lease == nil {
		return fmt.Errorf("%w: acquired leader lock returned nil lease", ErrInvalidController)
	}
	defer func() {
		if err := lease.Release(); err != nil && cycleErr == nil {
			cycleErr = fmt.Errorf("release fleet leader lock for %q: %w", c.config.GroupID, err)
		}
	}()

	now := c.clock.Now()
	snapshot, err := c.source.Snapshot(ctx, c.config.GroupID)
	if err != nil {
		return fmt.Errorf("read fleet snapshot for %q: %w", c.config.GroupID, err)
	}
	providerState, err := c.provider.Describe(ctx, c.config.GroupID)
	if err != nil {
		return fmt.Errorf("read provider capacity for %q: %w", c.config.GroupID, err)
	}
	if err := mergeProviderPending(&snapshot, providerState); err != nil {
		return fmt.Errorf("merge provider capacity for %q: %w", c.config.GroupID, err)
	}
	snapshot.Inputs.Now = now
	snapshot.Inputs.UnderutilizedSince = c.underutilizedSince
	decision, err := c.planner.Plan(snapshot.Inputs)
	if err != nil {
		return fmt.Errorf("plan fleet %q: %w", c.config.GroupID, err)
	}
	if decision.PlannedWorkers > decision.DesiredWorkers {
		if c.underutilizedSince.IsZero() {
			c.underutilizedSince = now
		}
	} else {
		c.underutilizedSince = time.Time{}
	}
	c.publish(ctx, snapshot, decision, OutcomePlanned, now)

	var actionErr error
	switch decision.Action {
	case ActionNone:
		return nil
	case ActionLaunch:
		actionErr = c.launch(ctx, providerState, decision, now)
	case ActionBeginDrain:
		actionErr = c.beginDrain(ctx, decision)
	case ActionFinishTermination:
		actionErr = c.finishTermination(ctx, decision, now)
	default:
		actionErr = fmt.Errorf("%w: unknown planner action %d", ErrInvalidController, decision.Action)
	}
	if actionErr != nil {
		if errors.Is(actionErr, errProviderMutationInProgress) {
			return nil
		}
		c.publish(ctx, snapshot, decision, OutcomeRetrying, now)
		return actionErr
	}
	c.publish(ctx, snapshot, decision, OutcomeConfirmed, now)
	return nil
}

func (c *Controller) launch(ctx context.Context, state ProviderState, decision Decision, now time.Time) error {
	// The planner has already counted provider desired capacity as pending
	// supply. Add only its bounded step and set an absolute target so retries
	// cannot amplify a response-loss mutation.
	target := state.Desired + decision.LaunchCount
	if state.Desired != target {
		if err := c.source.RecordMutationIntent(ctx, c.config.GroupID, ConfirmedAction{Action: ActionLaunch, Desired: target, At: now}); err != nil {
			return fmt.Errorf("record desired-capacity mutation intent: %w", err)
		}
		mutationErr := c.provider.SetDesired(ctx, c.config.GroupID, target)
		var err error
		state, err = c.provider.Describe(ctx, c.config.GroupID)
		if err != nil {
			if mutationErr != nil {
				return fmt.Errorf("set desired to %d: %w (reconcile: %v)", target, mutationErr, err)
			}
			return fmt.Errorf("reconcile desired target %d: %w", target, err)
		}
		if state.Desired != target {
			if mutationErr != nil {
				return fmt.Errorf("%w: desired=%d target=%d after mutation error: %v", ErrProviderNotConverged, state.Desired, target, mutationErr)
			}
			return fmt.Errorf("%w: desired=%d target=%d", ErrProviderNotConverged, state.Desired, target)
		}
	}
	return c.recordConfirmed(ctx, ConfirmedAction{Action: ActionLaunch, Desired: target, At: now})
}

func (c *Controller) beginDrain(ctx context.Context, decision Decision) error {
	if decision.WorkerID == "" {
		return fmt.Errorf("%w: drain decision has no exact worker", ErrInvalidController)
	}
	mutationErr := c.source.MarkDraining(ctx, c.config.GroupID, decision.WorkerID)
	proof, readErr := c.source.TerminationProof(ctx, c.config.GroupID, decision.WorkerID)
	if readErr != nil {
		if mutationErr != nil {
			return fmt.Errorf("mark worker %q draining: %w (reconcile: %v)", decision.WorkerID, mutationErr, readErr)
		}
		return fmt.Errorf("reconcile drain for worker %q: %w", decision.WorkerID, readErr)
	}
	if proof.WorkerID != decision.WorkerID || (proof.State != WorkerDraining && proof.State != WorkerDisabled) {
		if mutationErr != nil {
			return fmt.Errorf("%w: worker %q drain not confirmed after error: %v", ErrProviderNotConverged, decision.WorkerID, mutationErr)
		}
		return fmt.Errorf("%w: worker %q drain not confirmed", ErrProviderNotConverged, decision.WorkerID)
	}
	return nil
}

func (c *Controller) finishTermination(ctx context.Context, decision Decision, now time.Time) error {
	if decision.WorkerID == "" {
		return fmt.Errorf("%w: termination decision has no exact worker", ErrUnsafeTermination)
	}
	proof, err := c.source.ClaimTermination(ctx, c.config.GroupID, decision.WorkerID)
	if err != nil {
		return fmt.Errorf("claim zero-authority termination for worker %q: %w", decision.WorkerID, err)
	}
	graceful := proof.State == WorkerDisabled && proof.LocalCleanupComplete
	fenced := (proof.State == WorkerLost || proof.State == WorkerDisabled) && proof.FencedForTermination
	if proof.WorkerID != decision.WorkerID || proof.ResourceID == "" || proof.AuthorityCount != 0 || (!graceful && !fenced) {
		return fmt.Errorf("%w: worker %q lacks graceful cleanup or fenced-loss zero-authority proof", ErrUnsafeTermination, decision.WorkerID)
	}

	state, err := c.provider.Describe(ctx, c.config.GroupID)
	if err != nil {
		return fmt.Errorf("describe provider before terminating worker %q: %w", decision.WorkerID, err)
	}
	initialDesired := state.Desired
	if err := c.source.RecordMutationIntent(ctx, c.config.GroupID, ConfirmedAction{Action: ActionFinishTermination, WorkerID: proof.WorkerID, ResourceID: proof.ResourceID, Desired: decision.DesiredWorkers, At: now}); err != nil {
		return fmt.Errorf("record termination mutation intent for worker %q: %w", decision.WorkerID, err)
	}
	instance, exists := state.Instances[proof.ResourceID]
	if !exists {
		if state.Desired > decision.DesiredWorkers {
			mutationErr := c.provider.SetDesired(ctx, c.config.GroupID, decision.DesiredWorkers)
			state, err = c.provider.Describe(ctx, c.config.GroupID)
			if err != nil {
				if mutationErr != nil {
					return fmt.Errorf("reconcile desired after absent worker %q: %w (mutation: %v)", proof.ResourceID, err, mutationErr)
				}
				return fmt.Errorf("reconcile desired after absent worker %q: %w", proof.ResourceID, err)
			}
			if _, reappeared := state.Instances[proof.ResourceID]; reappeared || state.Desired > decision.DesiredWorkers {
				if mutationErr != nil {
					return fmt.Errorf("%w: absent worker %q desired target did not converge after error: %v", ErrProviderNotConverged, proof.ResourceID, mutationErr)
				}
				return fmt.Errorf("%w: absent worker %q desired target did not converge", ErrProviderNotConverged, proof.ResourceID)
			}
		}
		return c.recordConfirmed(ctx, ConfirmedAction{Action: ActionFinishTermination, WorkerID: proof.WorkerID, ResourceID: proof.ResourceID, Desired: state.Desired, At: now})
	}
	if strings.HasPrefix(instance.Lifecycle, "Terminating") && state.Desired <= decision.DesiredWorkers {
		return errProviderMutationInProgress
	}
	if instance.ProtectedFromScaleIn {
		mutationErr := c.provider.SetScaleInProtection(ctx, c.config.GroupID, proof.ResourceID, false)
		state, err = c.provider.Describe(ctx, c.config.GroupID)
		if err != nil {
			if mutationErr != nil {
				return fmt.Errorf("unprotect worker %q: %w (reconcile: %v)", proof.ResourceID, mutationErr, err)
			}
			return fmt.Errorf("reconcile protection for worker %q: %w", proof.ResourceID, err)
		}
		instance, exists = state.Instances[proof.ResourceID]
		if !exists {
			if state.Desired >= initialDesired {
				return fmt.Errorf("%w: worker %q disappeared without confirmed desired decrement", ErrProviderNotConverged, proof.ResourceID)
			}
			return c.recordConfirmed(ctx, ConfirmedAction{Action: ActionFinishTermination, WorkerID: proof.WorkerID, ResourceID: proof.ResourceID, Desired: state.Desired, At: now})
		}
		if instance.ProtectedFromScaleIn {
			if mutationErr != nil {
				return fmt.Errorf("%w: worker %q remains protected after error: %v", ErrProviderNotConverged, proof.ResourceID, mutationErr)
			}
			return fmt.Errorf("%w: worker %q remains protected", ErrProviderNotConverged, proof.ResourceID)
		}
	}

	targetDesired := state.Desired
	if targetDesired > 0 {
		targetDesired--
	}
	mutationErr := c.provider.Terminate(ctx, c.config.GroupID, proof.ResourceID, true)
	state, err = c.provider.Describe(ctx, c.config.GroupID)
	if err != nil {
		if mutationErr != nil {
			return fmt.Errorf("terminate worker %q: %w (reconcile: %v)", proof.ResourceID, mutationErr, err)
		}
		return fmt.Errorf("reconcile termination of worker %q: %w", proof.ResourceID, err)
	}
	_, exists = state.Instances[proof.ResourceID]
	if exists || state.Desired > targetDesired {
		if mutationErr != nil {
			return fmt.Errorf("%w: worker %q still present or desired not decremented after error: %v", ErrProviderNotConverged, proof.ResourceID, mutationErr)
		}
		return fmt.Errorf("%w: worker %q still present or desired not decremented", ErrProviderNotConverged, proof.ResourceID)
	}
	return c.recordConfirmed(ctx, ConfirmedAction{Action: ActionFinishTermination, WorkerID: proof.WorkerID, ResourceID: proof.ResourceID, Desired: state.Desired, At: now})
}

func (c *Controller) recordConfirmed(ctx context.Context, action ConfirmedAction) error {
	if err := c.source.RecordConfirmed(ctx, c.config.GroupID, action); err != nil {
		return fmt.Errorf("record confirmed action for %q: %w", c.config.GroupID, err)
	}
	return nil
}

// mergeProviderPending closes the enrollment visibility gap. Auto Scaling
// desired capacity and instances that have not yet acquired a Helmr identity
// are real billable, planned supply and must count toward pending and maximum
// limits. Exact resource mappings prevent double counting after enrollment.
func mergeProviderPending(snapshot *GroupSnapshot, state ProviderState) error {
	if snapshot == nil || state.Desired < 0 {
		return fmt.Errorf("%w: invalid provider snapshot", ErrInvalidController)
	}
	logicalIDs := make(map[string]struct{}, len(snapshot.Inputs.Workers))
	for _, worker := range snapshot.Inputs.Workers {
		logicalIDs[worker.ID] = struct{}{}
	}
	representedResources := make(map[string]struct{}, len(snapshot.ResourceIDs))
	for workerID, resourceID := range snapshot.ResourceIDs {
		if _, exists := logicalIDs[workerID]; !exists {
			return fmt.Errorf("%w: resource mapping references unknown worker %q", ErrInvalidController, workerID)
		}
		if resourceID == "" {
			return fmt.Errorf("%w: worker %q has an empty provider resource ID", ErrInvalidController, workerID)
		}
		if _, duplicate := representedResources[resourceID]; duplicate {
			return fmt.Errorf("%w: provider resource %q is mapped more than once", ErrInvalidController, resourceID)
		}
		representedResources[resourceID] = struct{}{}
	}
	// A provider-absent row normally cannot count as supply. Preserve the exact
	// zero-authority termination candidate until its provider-termination
	// acknowledgement commits, however. This makes a lost DB response after a
	// successful provider mutation retryable instead of orphaning the durable
	// group-removal fence forever.
	terminationCandidateID := snapshot.Inputs.TerminationCandidateID
	presentWorkers := make([]Worker, 0, len(snapshot.Inputs.Workers))
	presentMappings := make(map[string]string, len(snapshot.ResourceIDs))
	for _, worker := range snapshot.Inputs.Workers {
		resourceID, mapped := snapshot.ResourceIDs[worker.ID]
		if !mapped {
			return fmt.Errorf("%w: worker %q has no provider resource mapping", ErrInvalidController, worker.ID)
		}
		if _, present := state.Instances[resourceID]; !present && worker.ID != terminationCandidateID {
			continue
		}
		presentWorkers = append(presentWorkers, worker)
		presentMappings[worker.ID] = resourceID
	}
	snapshot.Inputs.Workers = presentWorkers
	snapshot.ResourceIDs = presentMappings
	if _, present := presentMappings[snapshot.Inputs.TerminationCandidateID]; !present {
		snapshot.Inputs.TerminationCandidateID = ""
	}
	hasPresentDrain := false
	for _, worker := range presentWorkers {
		if worker.State == WorkerDraining || worker.State == WorkerDisabled {
			hasPresentDrain = true
			break
		}
	}
	if !hasPresentDrain {
		snapshot.OldestDrainStartedAt = time.Time{}
	}
	representedResources = make(map[string]struct{}, len(presentMappings))
	for _, resourceID := range presentMappings {
		representedResources[resourceID] = struct{}{}
	}

	providerIDs := make([]string, 0, len(state.Instances))
	for resourceID := range state.Instances {
		if resourceID == "" {
			return fmt.Errorf("%w: provider returned an empty instance ID", ErrInvalidController)
		}
		providerIDs = append(providerIDs, resourceID)
	}
	sort.Strings(providerIDs)
	for _, resourceID := range providerIDs {
		if _, represented := representedResources[resourceID]; represented {
			continue
		}
		snapshot.Inputs.Workers = append(snapshot.Inputs.Workers, Worker{
			ID: uniqueSyntheticWorkerID("provider-instance:"+resourceID, logicalIDs), State: WorkerPending,
		})
	}

	// Desired capacity can lead the enumerated instance list while AWS is
	// launching. Represent each not-yet-materialized slot independently.
	for index := len(state.Instances); index < state.Desired; index++ {
		base := fmt.Sprintf("provider-launch-slot:%d", index)
		snapshot.Inputs.Workers = append(snapshot.Inputs.Workers, Worker{
			ID: uniqueSyntheticWorkerID(base, logicalIDs), State: WorkerPending,
		})
	}
	return nil
}

func uniqueSyntheticWorkerID(base string, existing map[string]struct{}) string {
	candidate := base
	for suffix := 1; ; suffix++ {
		if _, exists := existing[candidate]; !exists {
			existing[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s:%d", base, suffix)
	}
}

func (c *Controller) publish(ctx context.Context, snapshot GroupSnapshot, decision Decision, outcome Outcome, now time.Time) {
	drainAge := time.Duration(0)
	queueAge := time.Duration(0)
	if !snapshot.OldestDrainStartedAt.IsZero() && now.After(snapshot.OldestDrainStartedAt) {
		drainAge = now.Sub(snapshot.OldestDrainStartedAt)
	}
	if drainAge >= c.config.DrainTimeout {
		// A timed-out drain is observable and retryable, but never permission to
		// bypass the durable disabled/zero-authority/local-cleanup proof.
		outcome = OutcomeRetrying
	}
	if !snapshot.Inputs.Demand.OldestQueuedAt.IsZero() && now.After(snapshot.Inputs.Demand.OldestQueuedAt) {
		queueAge = now.Sub(snapshot.Inputs.Demand.OldestQueuedAt)
	}
	metricCtx, cancel := context.WithTimeout(ctx, c.config.MetricsTimeout)
	defer cancel()
	_ = c.metrics.Publish(metricCtx, FleetMetrics{
		GroupID: c.config.GroupID, UncappedRequired: decision.UncappedRequiredWorkers,
		UnmetDeficit: decision.UnmetRequiredWorkers, CapReason: decision.CapReason,
		Desired: decision.DesiredWorkers, Supply: decision.PlannedWorkers,
		Pending: decision.PendingWorkers, Billable: decision.BillableWorkers,
		UncertifiedRunLaunchAttestations: snapshot.Inputs.UncertifiedRunLaunchAttestations,
		BootstrapPending:                 snapshot.Inputs.UncertifiedRunLaunchAttestations > 0,
		Action:                           decision.Action, Outcome: outcome, DrainAge: drainAge, QueueAge: queueAge,
		DrainTimedOut: drainAge >= c.config.DrainTimeout,
	})
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type timerSleeper struct{}

func (timerSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func doubledDurationBounded(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return current * 2
}
