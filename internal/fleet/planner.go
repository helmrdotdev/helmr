// Package fleet contains deterministic fleet-capacity planning. It deliberately
// has no provider, database, or metrics dependencies so every controller can
// apply the same policy to an authoritative demand snapshot.
package fleet

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"time"
)

// MicroUSD represents one millionth of a US dollar for cost reporting.
type MicroUSD uint64

type Capacity struct {
	MilliCPU           uint64
	MemoryBytes        uint64
	WorkloadDiskBytes  uint64
	ScratchBytes       uint64
	BuildCacheBytes    uint64
	ArtifactCacheBytes uint64
	VMSlots            uint64
	BuildExecutors     uint64
}

// WorkloadBucket preserves indivisible workload shape. Count workloads in a
// bucket have the same resource shape and compatibility requirements.
type WorkloadBucket struct {
	CompatibilityKey string
	Shape            Capacity
	Count            uint64
}

// Demand is an authoritative, provider-independent demand snapshot. Keeping
// active and queued work as shape buckets prevents aggregate resource sums
// from claiming combinations that cannot fit on real instances.
type Demand struct {
	Active         []WorkloadBucket
	Queued         []WorkloadBucket
	OldestQueuedAt time.Time
}

// Policy is one independent worker-group policy. Run and build controllers
// should each own a separate Planner even when their policy values match.
type Policy struct {
	MinWorkers               int
	WarmWorkers              int
	MaxWorkers               int
	InstanceCapacity         Capacity
	AllowedCompatibilityKeys []string

	MaxScaleOutPerCycle int
	MaxPendingWorkers   int
	MaxPackingItems     int
	ScaleOutCooldown    time.Duration
	ScaleInCooldown     time.Duration
	ScaleInHysteresis   time.Duration
	EmergencyStop       bool
}

type WorkerState uint8

const (
	WorkerPending WorkerState = iota + 1
	WorkerActive
	WorkerDraining
	WorkerDisabled
	WorkerLost
)

// Worker describes one still-billable fleet member. Pending workers count as
// planned supply so repeated controller cycles cannot relaunch the same
// deficit. Draining and disabled workers count against hard cost limits but do
// not provide supply.
type Worker struct {
	ID                   string
	State                WorkerState
	AuthorityCount       uint64
	ActivatedAt          time.Time
	LocalCleanupComplete bool
	FencedForTermination bool
}

type Inputs struct {
	Now     time.Time
	Demand  Demand
	Workers []Worker
	// UncertifiedRunLaunchAttestations is the number of current launch
	// attestations that have never supplied a certified run runtime identity.
	// Historical certifications remain valid after a worker is disabled or its
	// provider instance terminates, matching run routing's identity authority.
	UncertifiedRunLaunchAttestations int
	LastScaleOutAt                   time.Time
	LastScaleInAt                    time.Time
	UnderutilizedSince               time.Time

	// TerminationCandidateID is the exact worker selected by the surrounding
	// lifecycle operation. The planner may finish termination only for this ID.
	TerminationCandidateID string
}

type Action uint8

const (
	ActionNone Action = iota
	ActionLaunch
	ActionBeginDrain
	ActionFinishTermination
)

type Reason string

const (
	ReasonAtDesiredCapacity       Reason = "at_desired_capacity"
	ReasonLaunchCapacity          Reason = "launch_capacity"
	ReasonEmergencyStop           Reason = "emergency_stop"
	ReasonScaleOutCooldown        Reason = "scale_out_cooldown"
	ReasonPendingLimit            Reason = "pending_limit"
	ReasonHardCapacityLimit       Reason = "hard_capacity_limit"
	ReasonScaleInCooldown         Reason = "scale_in_cooldown"
	ReasonScaleInHysteresis       Reason = "scale_in_hysteresis"
	ReasonDrainInProgress         Reason = "drain_in_progress"
	ReasonBeginDrain              Reason = "begin_drain"
	ReasonTerminationNotReady     Reason = "termination_not_ready"
	ReasonFinishTermination       Reason = "finish_termination"
	ReasonPendingSupplyConverging Reason = "pending_supply_converging"
	ReasonMaximumSaturated        Reason = "maximum_saturated"
)

type CapReason string

const (
	CapNone    CapReason = ""
	CapMaximum CapReason = "maximum"
)

type DimensionCeilings struct {
	MilliCPU           int
	MemoryBytes        int
	WorkloadDiskBytes  int
	ScratchBytes       int
	BuildCacheBytes    int
	ArtifactCacheBytes int
	VMSlots            int
	BuildExecutors     int
}

type CompatibilityRequirement struct {
	CompatibilityKey string
	Workers          int
}

// Decision is a complete deterministic planning result. LaunchCount is set
// only for ActionLaunch; WorkerID is set only for drain/termination actions.
type Decision struct {
	Action                    Action
	Reason                    Reason
	DesiredWorkers            int
	UncappedRequiredWorkers   int
	UnmetRequiredWorkers      int
	CapReason                 CapReason
	PlannedWorkers            int
	ActiveWorkers             int
	PendingWorkers            int
	BillableWorkers           int
	LaunchCount               int
	WorkerID                  string
	RequiredByDimension       DimensionCeilings
	RequiredByCompatibility   []CompatibilityRequirement
	ConservativePackedWorkers int
}

type Planner struct {
	policy               Policy
	allowedCompatibility map[string]struct{}
}

var (
	ErrInvalidPolicy = errors.New("invalid fleet policy")
	ErrInvalidInputs = errors.New("invalid fleet inputs")
	ErrPackingLimit  = errors.New("fleet packing item limit exceeded")
)

func NewPlanner(policy Policy) (*Planner, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	policy.AllowedCompatibilityKeys = append([]string(nil), policy.AllowedCompatibilityKeys...)
	allowed := make(map[string]struct{}, len(policy.AllowedCompatibilityKeys))
	for _, key := range policy.AllowedCompatibilityKeys {
		allowed[key] = struct{}{}
	}
	return &Planner{policy: policy, allowedCompatibility: allowed}, nil
}

func (p *Planner) Plan(in Inputs) (Decision, error) {
	if p == nil {
		return Decision{}, fmt.Errorf("%w: nil planner", ErrInvalidPolicy)
	}
	if err := validateInputs(in); err != nil {
		return Decision{}, err
	}

	ceilings, compatibilityRequirements, required, err := p.workloadRequirements(in.Demand)
	if err != nil {
		return Decision{}, err
	}

	bootstrapFloor := 0
	if in.UncertifiedRunLaunchAttestations > 0 {
		bootstrapFloor = 1
	}
	floor := maxInt(p.policy.MinWorkers, p.policy.WarmWorkers, bootstrapFloor)
	uncappedRequired := maxInt(required, floor)
	desired := uncappedRequired
	desired = minInt(desired, p.policy.MaxWorkers)
	capReason := CapNone
	if uncappedRequired > desired {
		capReason = CapMaximum
	}

	decision := Decision{
		Action:                    ActionNone,
		Reason:                    ReasonAtDesiredCapacity,
		DesiredWorkers:            desired,
		UncappedRequiredWorkers:   uncappedRequired,
		CapReason:                 capReason,
		RequiredByDimension:       ceilings,
		RequiredByCompatibility:   compatibilityRequirements,
		ConservativePackedWorkers: required,
	}

	workerByID := make(map[string]Worker, len(in.Workers))
	active := make([]Worker, 0, len(in.Workers))
	drainInProgress := false
	for _, worker := range in.Workers {
		workerByID[worker.ID] = worker
		decision.BillableWorkers++
		switch worker.State {
		case WorkerPending:
			decision.PendingWorkers++
			decision.PlannedWorkers++
		case WorkerActive:
			decision.ActiveWorkers++
			decision.PlannedWorkers++
			active = append(active, worker)
		case WorkerDraining, WorkerDisabled, WorkerLost:
			drainInProgress = true
		}
	}
	decision.UnmetRequiredWorkers = maxInt(0, uncappedRequired-decision.PlannedWorkers)

	if in.TerminationCandidateID != "" {
		candidate := workerByID[in.TerminationCandidateID]
		if candidate.AuthorityCount == 0 && ((candidate.State == WorkerDisabled && (candidate.LocalCleanupComplete || candidate.FencedForTermination)) || (candidate.State == WorkerLost && candidate.FencedForTermination)) {
			decision.Action = ActionFinishTermination
			decision.Reason = ReasonFinishTermination
			decision.WorkerID = candidate.ID
			return decision, nil
		}
	}

	if decision.PlannedWorkers == desired && capReason != CapNone {
		decision.Reason = ReasonMaximumSaturated
		return decision, nil
	}

	if decision.PlannedWorkers < desired {
		if p.policy.EmergencyStop {
			decision.Reason = ReasonEmergencyStop
			return decision, nil
		}
		if withinEitherCooldown(in.Now, in.LastScaleOutAt, in.LastScaleInAt, p.policy.ScaleOutCooldown) {
			decision.Reason = ReasonScaleOutCooldown
			return decision, nil
		}

		launch := desired - decision.PlannedWorkers
		launch = minInt(launch, p.policy.MaxScaleOutPerCycle)
		launch = minInt(launch, p.policy.MaxPendingWorkers-decision.PendingWorkers)
		launch = minInt(launch, p.policy.MaxWorkers-decision.BillableWorkers)
		if launch <= 0 {
			switch {
			case decision.PendingWorkers >= p.policy.MaxPendingWorkers:
				decision.Reason = ReasonPendingLimit
			default:
				decision.Reason = ReasonHardCapacityLimit
			}
			return decision, nil
		}
		decision.Action = ActionLaunch
		decision.Reason = ReasonLaunchCapacity
		decision.LaunchCount = launch
		return decision, nil
	}

	if decision.PlannedWorkers > desired {
		// Pending workers are never selected for scale-in. Let them converge so
		// the controller can reconsider using certified active capacity.
		if len(active) <= desired {
			decision.Reason = ReasonPendingSupplyConverging
			return decision, nil
		}
		if drainInProgress {
			decision.Reason = ReasonDrainInProgress
			return decision, nil
		}
		if withinEitherCooldown(in.Now, in.LastScaleOutAt, in.LastScaleInAt, p.policy.ScaleInCooldown) {
			decision.Reason = ReasonScaleInCooldown
			return decision, nil
		}
		if p.policy.ScaleInHysteresis > 0 && (in.UnderutilizedSince.IsZero() || in.Now.Sub(in.UnderutilizedSince) < p.policy.ScaleInHysteresis) {
			decision.Reason = ReasonScaleInHysteresis
			return decision, nil
		}

		sort.Slice(active, func(i, j int) bool {
			if active[i].AuthorityCount != active[j].AuthorityCount {
				return active[i].AuthorityCount < active[j].AuthorityCount
			}
			if !active[i].ActivatedAt.Equal(active[j].ActivatedAt) {
				return active[i].ActivatedAt.Before(active[j].ActivatedAt)
			}
			return active[i].ID < active[j].ID
		})
		decision.Action = ActionBeginDrain
		decision.Reason = ReasonBeginDrain
		decision.WorkerID = active[0].ID
		return decision, nil
	}

	if in.TerminationCandidateID != "" {
		decision.Reason = ReasonTerminationNotReady
	}
	return decision, nil
}

func CostForWorkers(workers int, hourlyPerInstance MicroUSD) (MicroUSD, error) {
	if workers < 0 {
		return 0, fmt.Errorf("%w: workers must not be negative", ErrInvalidInputs)
	}
	if hourlyPerInstance == 0 {
		return 0, fmt.Errorf("%w: hourly fixture price must be positive", ErrInvalidPolicy)
	}
	if uint64(workers) > math.MaxUint64/uint64(hourlyPerInstance) {
		return 0, fmt.Errorf("%w: hourly cost overflow", ErrInvalidInputs)
	}
	return MicroUSD(uint64(workers) * uint64(hourlyPerInstance)), nil
}

func validatePolicy(policy Policy) error {
	if policy.MinWorkers < 0 || policy.WarmWorkers < 0 || policy.MaxWorkers < 0 {
		return fmt.Errorf("%w: worker bounds must not be negative", ErrInvalidPolicy)
	}
	if policy.MinWorkers > policy.MaxWorkers || policy.WarmWorkers > policy.MaxWorkers {
		return fmt.Errorf("%w: min and warm workers must not exceed max workers", ErrInvalidPolicy)
	}
	if policy.MaxScaleOutPerCycle <= 0 {
		return fmt.Errorf("%w: max scale-out per cycle must be positive", ErrInvalidPolicy)
	}
	if policy.MaxPendingWorkers < 0 {
		return fmt.Errorf("%w: max pending workers must not be negative", ErrInvalidPolicy)
	}
	if policy.MaxPackingItems <= 0 {
		return fmt.Errorf("%w: max packing items must be positive", ErrInvalidPolicy)
	}
	if policy.ScaleOutCooldown < 0 || policy.ScaleInCooldown < 0 || policy.ScaleInHysteresis < 0 {
		return fmt.Errorf("%w: durations must not be negative", ErrInvalidPolicy)
	}
	if policy.InstanceCapacity.isZero() {
		return fmt.Errorf("%w: certified per-instance capacity is required", ErrInvalidPolicy)
	}
	if len(policy.AllowedCompatibilityKeys) == 0 {
		return fmt.Errorf("%w: at least one compatibility key is required", ErrInvalidPolicy)
	}
	seenCompatibility := make(map[string]struct{}, len(policy.AllowedCompatibilityKeys))
	for _, key := range policy.AllowedCompatibilityKeys {
		if key == "" {
			return fmt.Errorf("%w: compatibility keys must not be empty", ErrInvalidPolicy)
		}
		if _, exists := seenCompatibility[key]; exists {
			return fmt.Errorf("%w: duplicate compatibility key %q", ErrInvalidPolicy, key)
		}
		seenCompatibility[key] = struct{}{}
	}
	return nil
}

func validateInputs(in Inputs) error {
	if in.Now.IsZero() {
		return fmt.Errorf("%w: current time is required", ErrInvalidInputs)
	}
	if in.UncertifiedRunLaunchAttestations < 0 {
		return fmt.Errorf("%w: uncertified run attestation count must not be negative", ErrInvalidInputs)
	}
	if !in.Demand.OldestQueuedAt.IsZero() && in.Demand.OldestQueuedAt.After(in.Now) {
		return fmt.Errorf("%w: oldest queued time is in the future", ErrInvalidInputs)
	}
	for name, instant := range map[string]time.Time{
		"last scale-out":      in.LastScaleOutAt,
		"last scale-in":       in.LastScaleInAt,
		"underutilized since": in.UnderutilizedSince,
	} {
		if !instant.IsZero() && instant.After(in.Now) {
			return fmt.Errorf("%w: %s is in the future", ErrInvalidInputs, name)
		}
	}

	seen := make(map[string]struct{}, len(in.Workers))
	candidateFound := in.TerminationCandidateID == ""
	for _, worker := range in.Workers {
		if worker.ID == "" {
			return fmt.Errorf("%w: worker ID is required", ErrInvalidInputs)
		}
		if _, ok := seen[worker.ID]; ok {
			return fmt.Errorf("%w: duplicate worker ID %q", ErrInvalidInputs, worker.ID)
		}
		seen[worker.ID] = struct{}{}
		switch worker.State {
		case WorkerPending, WorkerActive, WorkerDraining, WorkerDisabled, WorkerLost:
		default:
			return fmt.Errorf("%w: invalid state for worker %q", ErrInvalidInputs, worker.ID)
		}
		if worker.ID == in.TerminationCandidateID {
			candidateFound = true
		}
		if worker.State == WorkerActive && worker.ActivatedAt.IsZero() {
			return fmt.Errorf("%w: active worker %q requires activation time", ErrInvalidInputs, worker.ID)
		}
		if !worker.ActivatedAt.IsZero() && worker.ActivatedAt.After(in.Now) {
			return fmt.Errorf("%w: activation time for worker %q is in the future", ErrInvalidInputs, worker.ID)
		}
	}
	if !candidateFound {
		return fmt.Errorf("%w: termination candidate %q is not in the worker snapshot", ErrInvalidInputs, in.TerminationCandidateID)
	}
	return nil
}

type workloadKey struct {
	compatibility string
	shape         Capacity
}

func (p *Planner) workloadRequirements(demand Demand) (DimensionCeilings, []CompatibilityRequirement, int, error) {
	buckets := make(map[workloadKey]uint64, len(demand.Active)+len(demand.Queued))
	for _, source := range [][]WorkloadBucket{demand.Active, demand.Queued} {
		for _, bucket := range source {
			if bucket.Count == 0 {
				return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: workload bucket count must be positive", ErrInvalidInputs)
			}
			if bucket.CompatibilityKey == "" {
				return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: workload compatibility key is required", ErrInvalidInputs)
			}
			if _, allowed := p.allowedCompatibility[bucket.CompatibilityKey]; !allowed {
				return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: unsupported compatibility key %q", ErrInvalidInputs, bucket.CompatibilityKey)
			}
			if bucket.Shape.isZero() {
				return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: workload shape must not be empty", ErrInvalidInputs)
			}
			key := workloadKey{compatibility: bucket.CompatibilityKey, shape: bucket.Shape}
			if math.MaxUint64-buckets[key] < bucket.Count {
				return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: workload count overflow", ErrInvalidInputs)
			}
			buckets[key] += bucket.Count
		}
	}

	keys := make([]workloadKey, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].compatibility != keys[j].compatibility {
			return keys[i].compatibility < keys[j].compatibility
		}
		return capacityLess(keys[i].shape, keys[j].shape)
	})

	var aggregate Capacity
	keysByCompatibility := make(map[string][]workloadKey)
	for _, key := range keys {
		count := buckets[key]
		bucketCapacity, err := multiplyCapacity(key.shape, count)
		if err != nil {
			return DimensionCeilings{}, nil, 0, err
		}
		aggregate, err = addCapacity(aggregate, bucketCapacity)
		if err != nil {
			return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: demand capacity overflow", ErrInvalidInputs)
		}

		if _, err := itemsPerWorker(key.shape, p.policy.InstanceCapacity); err != nil {
			return DimensionCeilings{}, nil, 0, err
		}
		keysByCompatibility[key.compatibility] = append(keysByCompatibility[key.compatibility], key)
	}

	ceilings, _, err := requiredWorkers(aggregate, p.policy.InstanceCapacity)
	if err != nil {
		return DimensionCeilings{}, nil, 0, err
	}
	compatibilityKeys := make([]string, 0, len(keysByCompatibility))
	for key := range keysByCompatibility {
		compatibilityKeys = append(compatibilityKeys, key)
	}
	sort.Strings(compatibilityKeys)
	requirements := make([]CompatibilityRequirement, 0, len(compatibilityKeys))
	packedWorkers := 0
	for _, compatibility := range compatibilityKeys {
		workers, err := p.packCompatibility(keysByCompatibility[compatibility], buckets)
		if err != nil {
			return DimensionCeilings{}, nil, 0, err
		}
		if workers > math.MaxInt-packedWorkers {
			return DimensionCeilings{}, nil, 0, fmt.Errorf("%w: packed worker requirement overflows int", ErrInvalidInputs)
		}
		packedWorkers += workers
		requirements = append(requirements, CompatibilityRequirement{CompatibilityKey: compatibility, Workers: workers})
	}
	return ceilings, requirements, packedWorkers, nil
}

func (p *Planner) packCompatibility(keys []workloadKey, buckets map[workloadKey]uint64) (int, error) {
	if len(keys) == 1 {
		fit, err := itemsPerWorker(keys[0].shape, p.policy.InstanceCapacity)
		if err != nil {
			return 0, err
		}
		count := buckets[keys[0]]
		hosts := count / fit
		if count%fit != 0 {
			hosts++
		}
		if hosts > uint64(math.MaxInt) {
			return 0, fmt.Errorf("%w: packed worker requirement overflows int", ErrInvalidInputs)
		}
		return int(hosts), nil
	}

	var itemCount uint64
	for _, key := range keys {
		count := buckets[key]
		if math.MaxUint64-itemCount < count {
			return 0, fmt.Errorf("%w: workload count overflow", ErrInvalidInputs)
		}
		itemCount += count
	}
	if itemCount > uint64(p.policy.MaxPackingItems) {
		return 0, fmt.Errorf("%w: compatibility %q has %d mixed-shape items, limit %d", ErrPackingLimit, keys[0].compatibility, itemCount, p.policy.MaxPackingItems)
	}

	items := make([]Capacity, 0, int(itemCount))
	for _, key := range keys {
		for count := uint64(0); count < buckets[key]; count++ {
			items = append(items, key.shape)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if comparison := compareDominantRatio(items[i], items[j], p.policy.InstanceCapacity); comparison != 0 {
			return comparison > 0
		}
		return capacityLess(items[i], items[j])
	})

	bins := make([]Capacity, 0, minInt(len(items), p.policy.MaxWorkers))
	for _, item := range items {
		placed := false
		for index := range bins {
			if capacityFits(item, bins[index]) {
				bins[index] = subtractCapacity(bins[index], item)
				placed = true
				break
			}
		}
		if !placed {
			bins = append(bins, subtractCapacity(p.policy.InstanceCapacity, item))
		}
	}
	return len(bins), nil
}

func itemsPerWorker(shape, instance Capacity) (uint64, error) {
	fit := uint64(math.MaxUint64)
	dimensions := []struct {
		name     string
		shape    uint64
		capacity uint64
	}{
		{"cpu", shape.MilliCPU, instance.MilliCPU},
		{"memory", shape.MemoryBytes, instance.MemoryBytes},
		{"workload disk", shape.WorkloadDiskBytes, instance.WorkloadDiskBytes},
		{"scratch", shape.ScratchBytes, instance.ScratchBytes},
		{"build cache", shape.BuildCacheBytes, instance.BuildCacheBytes},
		{"artifact cache", shape.ArtifactCacheBytes, instance.ArtifactCacheBytes},
		{"vm slots", shape.VMSlots, instance.VMSlots},
		{"build executors", shape.BuildExecutors, instance.BuildExecutors},
	}
	for _, dimension := range dimensions {
		if dimension.shape == 0 {
			continue
		}
		if dimension.capacity == 0 || dimension.shape > dimension.capacity {
			return 0, fmt.Errorf("%w: workload %s requirement does not fit one certified instance", ErrInvalidInputs, dimension.name)
		}
		fit = minUint64(fit, dimension.capacity/dimension.shape)
	}
	if fit == math.MaxUint64 || fit == 0 {
		return 0, fmt.Errorf("%w: workload has no usable resource dimension", ErrInvalidInputs)
	}
	return fit, nil
}

func multiplyCapacity(capacity Capacity, count uint64) (Capacity, error) {
	multiply := func(value uint64) (uint64, error) {
		if value != 0 && count > math.MaxUint64/value {
			return 0, fmt.Errorf("%w: demand capacity overflow", ErrInvalidInputs)
		}
		return value * count, nil
	}
	var result Capacity
	var err error
	if result.MilliCPU, err = multiply(capacity.MilliCPU); err != nil {
		return Capacity{}, err
	}
	if result.MemoryBytes, err = multiply(capacity.MemoryBytes); err != nil {
		return Capacity{}, err
	}
	if result.WorkloadDiskBytes, err = multiply(capacity.WorkloadDiskBytes); err != nil {
		return Capacity{}, err
	}
	if result.ScratchBytes, err = multiply(capacity.ScratchBytes); err != nil {
		return Capacity{}, err
	}
	if result.BuildCacheBytes, err = multiply(capacity.BuildCacheBytes); err != nil {
		return Capacity{}, err
	}
	if result.ArtifactCacheBytes, err = multiply(capacity.ArtifactCacheBytes); err != nil {
		return Capacity{}, err
	}
	if result.VMSlots, err = multiply(capacity.VMSlots); err != nil {
		return Capacity{}, err
	}
	if result.BuildExecutors, err = multiply(capacity.BuildExecutors); err != nil {
		return Capacity{}, err
	}
	return result, nil
}

func compareDominantRatio(left, right, instance Capacity) int {
	leftNumerator, leftDenominator := dominantRatio(left, instance)
	rightNumerator, rightDenominator := dominantRatio(right, instance)
	return compareFractions(leftNumerator, leftDenominator, rightNumerator, rightDenominator)
}

func dominantRatio(shape, instance Capacity) (uint64, uint64) {
	numerator, denominator := uint64(0), uint64(1)
	dimensions := [][2]uint64{
		{shape.MilliCPU, instance.MilliCPU},
		{shape.MemoryBytes, instance.MemoryBytes},
		{shape.WorkloadDiskBytes, instance.WorkloadDiskBytes},
		{shape.ScratchBytes, instance.ScratchBytes},
		{shape.BuildCacheBytes, instance.BuildCacheBytes},
		{shape.ArtifactCacheBytes, instance.ArtifactCacheBytes},
		{shape.VMSlots, instance.VMSlots},
		{shape.BuildExecutors, instance.BuildExecutors},
	}
	for _, dimension := range dimensions {
		if dimension[0] != 0 && compareFractions(dimension[0], dimension[1], numerator, denominator) > 0 {
			numerator, denominator = dimension[0], dimension[1]
		}
	}
	return numerator, denominator
}

func compareFractions(leftNumerator, leftDenominator, rightNumerator, rightDenominator uint64) int {
	leftHigh, leftLow := bits.Mul64(leftNumerator, rightDenominator)
	rightHigh, rightLow := bits.Mul64(rightNumerator, leftDenominator)
	if leftHigh < rightHigh || (leftHigh == rightHigh && leftLow < rightLow) {
		return -1
	}
	if leftHigh > rightHigh || (leftHigh == rightHigh && leftLow > rightLow) {
		return 1
	}
	return 0
}

func capacityFits(required, remaining Capacity) bool {
	return required.MilliCPU <= remaining.MilliCPU &&
		required.MemoryBytes <= remaining.MemoryBytes &&
		required.WorkloadDiskBytes <= remaining.WorkloadDiskBytes &&
		required.ScratchBytes <= remaining.ScratchBytes &&
		required.BuildCacheBytes <= remaining.BuildCacheBytes &&
		required.ArtifactCacheBytes <= remaining.ArtifactCacheBytes &&
		required.VMSlots <= remaining.VMSlots &&
		required.BuildExecutors <= remaining.BuildExecutors
}

func subtractCapacity(remaining, used Capacity) Capacity {
	return Capacity{
		MilliCPU:           remaining.MilliCPU - used.MilliCPU,
		MemoryBytes:        remaining.MemoryBytes - used.MemoryBytes,
		WorkloadDiskBytes:  remaining.WorkloadDiskBytes - used.WorkloadDiskBytes,
		ScratchBytes:       remaining.ScratchBytes - used.ScratchBytes,
		BuildCacheBytes:    remaining.BuildCacheBytes - used.BuildCacheBytes,
		ArtifactCacheBytes: remaining.ArtifactCacheBytes - used.ArtifactCacheBytes,
		VMSlots:            remaining.VMSlots - used.VMSlots,
		BuildExecutors:     remaining.BuildExecutors - used.BuildExecutors,
	}
}

func capacityLess(left, right Capacity) bool {
	leftValues := [...]uint64{left.MilliCPU, left.MemoryBytes, left.WorkloadDiskBytes, left.ScratchBytes, left.BuildCacheBytes, left.ArtifactCacheBytes, left.VMSlots, left.BuildExecutors}
	rightValues := [...]uint64{right.MilliCPU, right.MemoryBytes, right.WorkloadDiskBytes, right.ScratchBytes, right.BuildCacheBytes, right.ArtifactCacheBytes, right.VMSlots, right.BuildExecutors}
	for index := range leftValues {
		if leftValues[index] != rightValues[index] {
			return leftValues[index] > rightValues[index]
		}
	}
	return false
}

func requiredWorkers(demand, instance Capacity) (DimensionCeilings, int, error) {
	var ceilings DimensionCeilings
	values := []struct {
		name     string
		demand   uint64
		capacity uint64
		set      func(int)
	}{
		{"cpu", demand.MilliCPU, instance.MilliCPU, func(v int) { ceilings.MilliCPU = v }},
		{"memory", demand.MemoryBytes, instance.MemoryBytes, func(v int) { ceilings.MemoryBytes = v }},
		{"workload disk", demand.WorkloadDiskBytes, instance.WorkloadDiskBytes, func(v int) { ceilings.WorkloadDiskBytes = v }},
		{"scratch", demand.ScratchBytes, instance.ScratchBytes, func(v int) { ceilings.ScratchBytes = v }},
		{"build cache", demand.BuildCacheBytes, instance.BuildCacheBytes, func(v int) { ceilings.BuildCacheBytes = v }},
		{"artifact cache", demand.ArtifactCacheBytes, instance.ArtifactCacheBytes, func(v int) { ceilings.ArtifactCacheBytes = v }},
		{"vm slots", demand.VMSlots, instance.VMSlots, func(v int) { ceilings.VMSlots = v }},
		{"build executors", demand.BuildExecutors, instance.BuildExecutors, func(v int) { ceilings.BuildExecutors = v }},
	}
	required := 0
	for _, value := range values {
		if value.demand == 0 {
			continue
		}
		if value.capacity == 0 {
			return DimensionCeilings{}, 0, fmt.Errorf("%w: nonzero %s demand has zero certified per-instance capacity", ErrInvalidPolicy, value.name)
		}
		workers := value.demand / value.capacity
		if value.demand%value.capacity != 0 {
			workers++
		}
		if workers > uint64(math.MaxInt) {
			return DimensionCeilings{}, 0, fmt.Errorf("%w: %s worker requirement overflows int", ErrInvalidInputs, value.name)
		}
		value.set(int(workers))
		required = maxInt(required, int(workers))
	}
	return ceilings, required, nil
}

func addCapacity(a, b Capacity) (Capacity, error) {
	add := func(left, right uint64) (uint64, bool) {
		if right > math.MaxUint64-left {
			return 0, false
		}
		return left + right, true
	}
	var result Capacity
	var ok bool
	if result.MilliCPU, ok = add(a.MilliCPU, b.MilliCPU); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.MemoryBytes, ok = add(a.MemoryBytes, b.MemoryBytes); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.WorkloadDiskBytes, ok = add(a.WorkloadDiskBytes, b.WorkloadDiskBytes); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.ScratchBytes, ok = add(a.ScratchBytes, b.ScratchBytes); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.BuildCacheBytes, ok = add(a.BuildCacheBytes, b.BuildCacheBytes); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.ArtifactCacheBytes, ok = add(a.ArtifactCacheBytes, b.ArtifactCacheBytes); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.VMSlots, ok = add(a.VMSlots, b.VMSlots); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	if result.BuildExecutors, ok = add(a.BuildExecutors, b.BuildExecutors); !ok {
		return Capacity{}, ErrInvalidInputs
	}
	return result, nil
}

func (capacity Capacity) isZero() bool {
	return capacity == (Capacity{})
}

func withinCooldown(now, last time.Time, cooldown time.Duration) bool {
	return cooldown > 0 && !last.IsZero() && now.Sub(last) < cooldown
}

func withinEitherCooldown(now, lastOut, lastIn time.Time, cooldown time.Duration) bool {
	return withinCooldown(now, lastOut, cooldown) || withinCooldown(now, lastIn, cooldown)
}

func minInt(values ...int) int {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}

func maxInt(values ...int) int {
	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}
