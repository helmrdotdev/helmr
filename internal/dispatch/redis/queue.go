package redis

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	redisv9 "github.com/redis/go-redis/v9"
)

const (
	defaultPrefix           = "helmr:dispatch"
	defaultMessageSafetyTTL = 30 * 24 * time.Hour
	defaultOldestWorkAfter  = 30 * time.Second
)

// Queue is reconstructable and grants no lease, capacity, or completion
// authority; Postgres remains the placement authority.
type Queue struct {
	client     redisv9.Cmdable
	prefix     string
	messageTTL time.Duration
	now        func() time.Time

	regionMu      sync.Mutex
	regionCursors map[dispatch.WorkKind]uint64
	regionPending map[dispatch.WorkKind][]string
}

type Option func(*Queue)

func New(client redisv9.Cmdable, opts ...Option) (*Queue, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}
	q := &Queue{
		client: client, prefix: defaultPrefix, messageTTL: defaultMessageSafetyTTL, now: time.Now,
		regionCursors: map[dispatch.WorkKind]uint64{}, regionPending: map[dispatch.WorkKind][]string{},
	}
	for _, opt := range opts {
		opt(q)
	}
	if strings.TrimSpace(q.prefix) == "" || q.messageTTL <= 0 || q.now == nil {
		return nil, errors.New("redis ready-index configuration is invalid")
	}
	return q, nil
}

func WithPrefix(prefix string) Option {
	return func(q *Queue) { q.prefix = strings.TrimRight(prefix, ":") }
}

func WithMessageSafetyTTL(ttl time.Duration) Option {
	return func(q *Queue) { q.messageTTL = ttl }
}

func WithClock(now func() time.Time) Option {
	return func(q *Queue) { q.now = now }
}

func (q *Queue) Enqueue(ctx context.Context, message dispatch.Message) (dispatch.EnqueueResult, error) {
	if message.EnqueuedAt.IsZero() {
		message.EnqueuedAt = q.now().UTC()
	}
	if message.QueueTimestamp.IsZero() {
		message.QueueTimestamp = message.EnqueuedAt
	}
	if err := message.Validate(); err != nil {
		return dispatch.EnqueueResult{}, err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return dispatch.EnqueueResult{}, fmt.Errorf("encode ready-index entry: %w", err)
	}
	readyKey := q.readyKey(message)
	workID := message.WorkID()
	messageKey := q.sourceKey(message.WorkKind, workID)
	pipe := q.client.TxPipeline()
	pipe.Set(ctx, messageKey, payload, q.messageTTL)
	pipe.ZAdd(ctx, readyKey, redisv9.Z{Score: readyScore(message.Priority, message.QueueTimestamp), Member: workID})
	pipe.ZAdd(ctx, q.oldestKey(message.WorkKind, message.RegionID), redisv9.Z{Score: float64(message.QueueTimestamp.UTC().UnixMilli()), Member: workID})
	pipe.SAdd(ctx, q.regionsKey(message.WorkKind), message.RegionID)
	pipe.ZAddArgs(ctx, q.organizationsKey(message.WorkKind, message.RegionID), redisv9.ZAddArgs{NX: true, Members: []redisv9.Z{{Score: 0, Member: message.OrgID}}})
	pipe.ZAddArgs(ctx, q.environmentsKey(message.WorkKind, message.RegionID, message.OrgID), redisv9.ZAddArgs{NX: true, Members: []redisv9.Z{{Score: 0, Member: message.EnvironmentID}}})
	pipe.ZAddArgs(ctx, q.leavesKey(message.WorkKind, message.RegionID, message.OrgID, message.EnvironmentID), redisv9.ZAddArgs{NX: true, Members: []redisv9.Z{{Score: float64(message.QueueTimestamp.UTC().UnixMilli()), Member: q.leafID(message)}}})
	depth := pipe.ZCard(ctx, readyKey)
	for _, key := range []string{readyKey, q.oldestKey(message.WorkKind, message.RegionID), q.regionsKey(message.WorkKind),
		q.organizationsKey(message.WorkKind, message.RegionID), q.environmentsKey(message.WorkKind, message.RegionID, message.OrgID),
		q.leavesKey(message.WorkKind, message.RegionID, message.OrgID, message.EnvironmentID)} {
		pipe.Expire(ctx, key, q.messageTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return dispatch.EnqueueResult{}, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
	}
	return dispatch.EnqueueResult{QueueName: message.QueueName, MessageID: workID, Depth: depth.Val()}, nil
}

func (q *Queue) readyKey(message dispatch.Message) string {
	return q.prefix + ":ready:" + string(message.WorkKind) + ":leaf:" + q.leafID(message)
}

func (q *Queue) sourceKey(kind dispatch.WorkKind, workID string) string {
	return q.prefix + ":source:" + string(kind) + ":" + sanitizeKeyPart(workID)
}
func (q *Queue) regionsKey(kind dispatch.WorkKind) string {
	return q.prefix + ":ready:" + string(kind) + ":regions"
}
func (q *Queue) oldestKey(kind dispatch.WorkKind, region string) string {
	return q.prefix + ":ready:" + string(kind) + ":region:" + sanitizeKeyPart(region) + ":oldest"
}
func (q *Queue) organizationsKey(kind dispatch.WorkKind, region string) string {
	return q.prefix + ":ready:" + string(kind) + ":region:" + sanitizeKeyPart(region) + ":organizations"
}
func (q *Queue) environmentsKey(kind dispatch.WorkKind, region, org string) string {
	return q.prefix + ":ready:" + string(kind) + ":region:" + sanitizeKeyPart(region) + ":org:" + sanitizeKeyPart(org) + ":environments"
}
func (q *Queue) leavesKey(kind dispatch.WorkKind, region, org, environment string) string {
	return q.prefix + ":ready:" + string(kind) + ":region:" + sanitizeKeyPart(region) + ":org:" + sanitizeKeyPart(org) +
		":env:" + sanitizeKeyPart(environment) + ":leaves"
}
func (q *Queue) leafID(message dispatch.Message) string {
	scope := strings.Join([]string{string(message.WorkKind), message.RegionID, message.OrgID, message.ProjectID, message.EnvironmentID,
		message.QueueClass, message.QueueName}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(scope)))
}

func (q *Queue) ReadyRegions(ctx context.Context, kind dispatch.WorkKind, limit int64) ([]string, error) {
	if kind != dispatch.WorkKindRun && kind != dispatch.WorkKindBuild {
		return nil, errors.New("ready work kind must be run or build")
	}
	if limit <= 0 {
		limit = 32
	}
	q.regionMu.Lock()
	defer q.regionMu.Unlock()

	regions := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendPending := func() {
		pending := q.regionPending[kind]
		for len(pending) != 0 && int64(len(regions)) < limit {
			region := pending[0]
			pending = pending[1:]
			if _, duplicate := seen[region]; duplicate {
				continue
			}
			seen[region] = struct{}{}
			regions = append(regions, region)
		}
		q.regionPending[kind] = pending
	}
	appendPending()
	for int64(len(regions)) < limit {
		cursor := q.regionCursors[kind]
		batch, next, err := q.client.SScan(ctx, q.regionsKey(kind), cursor, "*", limit).Result()
		if err != nil {
			return nil, fmt.Errorf("%w: list ready regions: %v", dispatch.ErrQueueUnavailable, err)
		}
		q.regionCursors[kind] = next
		q.regionPending[kind] = append(q.regionPending[kind], batch...)
		appendPending()
		if next == 0 {
			// The next call starts a new bounded rotation only after every member
			// returned by this completed scan has been served from pending.
			break
		}
		if len(batch) == 0 {
			continue
		}
	}
	return regions, nil
}

func (q *Queue) SelectReady(ctx context.Context, selection dispatch.ReadySelection) ([]dispatch.Message, error) {
	if selection.WorkKind != dispatch.WorkKindRun && selection.WorkKind != dispatch.WorkKindBuild {
		return nil, errors.New("ready selection work kind must be run or build")
	}
	selection = normalizeSelection(selection)
	if strings.TrimSpace(selection.RegionID) == "" {
		return nil, errors.New("ready selection region is required")
	}
	selected := make([]dispatch.Message, 0, selection.Limit)
	seen := make(map[string]struct{}, selection.Limit)
	tenantContributions := make(map[string]int)

	// Old work bypasses virtual-finish order one item at a time. The scan is
	// bounded and Postgres still revalidates the returned candidate.
	oldest, err := q.client.ZRange(ctx, q.oldestKey(selection.WorkKind, selection.RegionID), 0, selection.OrganizationScanLimit-1).Result()
	if err != nil {
		return nil, fmt.Errorf("%w: scan oldest ready work: %v", dispatch.ErrQueueUnavailable, err)
	}
	for _, runID := range oldest {
		message, ok, err := q.loadMessage(ctx, selection.WorkKind, runID)
		if err != nil {
			return nil, err
		}
		if !ok {
			_ = q.removeUnknown(ctx, selection.WorkKind, selection.RegionID, "", runID)
			continue
		}
		if message.RegionID != selection.RegionID {
			_ = q.removeUnknown(ctx, selection.WorkKind, selection.RegionID, "", runID)
			continue
		}
		if q.now().UTC().Sub(message.QueueTimestamp) < selection.OldestWorkAfter {
			break
		}
		selected = append(selected, message)
		seen[message.WorkID()] = struct{}{}
		tenantContributions[message.OrgID]++
		if len(selected) == selection.Limit {
			return selected, nil
		}
		break
	}

	for len(selected) < selection.Limit {
		organizations, err := q.client.ZRange(ctx, q.organizationsKey(selection.WorkKind, selection.RegionID), 0, selection.OrganizationScanLimit-1).Result()
		if err != nil {
			return nil, fmt.Errorf("%w: scan ready organizations: %v", dispatch.ErrQueueUnavailable, err)
		}
		madeProgress := false
		for _, orgID := range organizations {
			if tenantContributions[orgID] >= selection.TenantContributionLimit {
				continue
			}
			message, ok, err := q.selectOrganizationCandidate(ctx, selection, orgID, seen)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			selected = append(selected, message)
			seen[message.WorkID()] = struct{}{}
			tenantContributions[orgID]++
			pipe := q.client.TxPipeline()
			pipe.ZIncrBy(ctx, q.organizationsKey(selection.WorkKind, selection.RegionID), 1, orgID)
			pipe.ZIncrBy(ctx, q.environmentsKey(selection.WorkKind, selection.RegionID, orgID), 1, message.EnvironmentID)
			// Move the selected leaf behind every currently ready sibling. Keeping
			// this update in the same MULTI/EXEC as the parent virtual-finish
			// updates bounds sibling starvation even while the selected leaf stays
			// continuously backlogged. Concurrent selectors can advance a leaf
			// more than once, but cannot make it jump ahead of an unserved sibling.
			const advanceLeafScript = `
local current = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not current then return 0 end
local tail = redis.call('ZRANGE', KEYS[1], -1, -1, 'WITHSCORES')
local next = tonumber(current) + 1
if #tail == 2 and tonumber(tail[2]) >= next then next = tonumber(tail[2]) + 1 end
redis.call('ZADD', KEYS[1], next, ARGV[1])
return next`
			pipe.Eval(ctx, advanceLeafScript, []string{
				q.leavesKey(selection.WorkKind, selection.RegionID, orgID, message.EnvironmentID),
			}, q.leafID(message))
			if _, err := pipe.Exec(ctx); err != nil {
				return nil, fmt.Errorf("%w: advance ready virtual finish: %v", dispatch.ErrQueueUnavailable, err)
			}
			madeProgress = true
			if len(selected) == selection.Limit {
				break
			}
		}
		if !madeProgress {
			break
		}
	}
	return selected, nil
}

func normalizeSelection(selection dispatch.ReadySelection) dispatch.ReadySelection {
	if selection.Limit <= 0 {
		selection.Limit = 32
	}
	if selection.OrganizationScanLimit <= 0 {
		selection.OrganizationScanLimit = 32
	}
	if selection.EnvironmentScanLimit <= 0 {
		selection.EnvironmentScanLimit = 8
	}
	if selection.LeafScanLimit <= 0 {
		selection.LeafScanLimit = 8
	}
	if selection.LeafContributionLimit <= 0 {
		selection.LeafContributionLimit = 4
	}
	if selection.TenantContributionLimit <= 0 {
		selection.TenantContributionLimit = 4
	}
	if selection.OldestWorkAfter <= 0 {
		selection.OldestWorkAfter = defaultOldestWorkAfter
	}
	return selection
}

func (q *Queue) selectOrganizationCandidate(ctx context.Context, selection dispatch.ReadySelection, orgID string, seen map[string]struct{}) (dispatch.Message, bool, error) {
	environments, err := q.client.ZRange(ctx, q.environmentsKey(selection.WorkKind, selection.RegionID, orgID), 0, selection.EnvironmentScanLimit-1).Result()
	if err != nil {
		return dispatch.Message{}, false, fmt.Errorf("%w: scan ready environments: %v", dispatch.ErrQueueUnavailable, err)
	}
	for _, environmentID := range environments {
		leaves, err := q.client.ZRange(ctx, q.leavesKey(selection.WorkKind, selection.RegionID, orgID, environmentID), 0, selection.LeafScanLimit-1).Result()
		if err != nil {
			return dispatch.Message{}, false, fmt.Errorf("%w: scan ready leaves: %v", dispatch.ErrQueueUnavailable, err)
		}
		for _, leafID := range leaves {
			runIDs, err := q.client.ZRange(ctx, q.prefix+":ready:"+string(selection.WorkKind)+":leaf:"+leafID, 0, selection.LeafContributionLimit-1).Result()
			if err != nil {
				return dispatch.Message{}, false, fmt.Errorf("%w: scan ready leaf: %v", dispatch.ErrQueueUnavailable, err)
			}
			for _, runID := range runIDs {
				if _, duplicate := seen[runID]; duplicate {
					continue
				}
				message, ok, err := q.loadMessage(ctx, selection.WorkKind, runID)
				if err != nil {
					return dispatch.Message{}, false, err
				}
				if !ok {
					_ = q.removeUnknown(ctx, selection.WorkKind, selection.RegionID, leafID, runID)
					continue
				}
				if message.RegionID != selection.RegionID || message.OrgID != orgID || message.EnvironmentID != environmentID || q.leafID(message) != leafID {
					staleRegion := ""
					if message.RegionID != selection.RegionID {
						staleRegion = selection.RegionID
					}
					_ = q.removeUnknown(ctx, selection.WorkKind, staleRegion, leafID, runID)
					continue
				}
				return message, true, nil
			}
		}
	}
	return dispatch.Message{}, false, nil
}

func (q *Queue) loadMessage(ctx context.Context, kind dispatch.WorkKind, workID string) (dispatch.Message, bool, error) {
	payload, err := q.client.Get(ctx, q.sourceKey(kind, workID)).Bytes()
	if errors.Is(err, redisv9.Nil) {
		return dispatch.Message{}, false, nil
	}
	if err != nil {
		return dispatch.Message{}, false, fmt.Errorf("%w: load ready source: %v", dispatch.ErrQueueUnavailable, err)
	}
	var message dispatch.Message
	if err := json.Unmarshal(payload, &message); err != nil || message.Validate() != nil || message.WorkKind != kind || message.WorkID() != workID {
		return dispatch.Message{}, false, nil
	}
	return message, true, nil
}

func (q *Queue) RemoveReady(ctx context.Context, kind dispatch.WorkKind, workID string, expectedFence string) error {
	if kind != dispatch.WorkKindRun && kind != dispatch.WorkKindBuild {
		return errors.New("ready work kind must be run or build")
	}
	message, ok, err := q.loadMessage(ctx, kind, workID)
	if err != nil {
		return err
	}
	if !ok {
		if err := q.client.Del(ctx, q.sourceKey(kind, workID)).Err(); err != nil {
			return fmt.Errorf("%w: remove invalid ready source: %v", dispatch.ErrQueueUnavailable, err)
		}
		return nil
	}
	if message.ReadyFence() != expectedFence {
		return nil
	}
	expectedPayload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode ready source for removal: %w", err)
	}
	const removeScript = `
local payload = redis.call('GET', KEYS[1])
if not payload then return 0 end
if payload ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[2])
redis.call('ZREM', KEYS[3], ARGV[2])
return 1`
	removed, err := q.client.Eval(ctx, removeScript, []string{
		q.sourceKey(kind, workID), q.readyKey(message), q.oldestKey(kind, message.RegionID),
	}, string(expectedPayload), workID).Int()
	if err != nil {
		return fmt.Errorf("%w: remove ready source: %v", dispatch.ErrQueueUnavailable, err)
	}
	if removed == 0 {
		return nil
	}
	return q.pruneEmptyHierarchy(ctx, message)
}

func (q *Queue) removeUnknown(ctx context.Context, kind dispatch.WorkKind, region, leafID, workID string) error {
	if region == "" && leafID == "" {
		return nil
	}
	pipe := q.client.TxPipeline()
	if region != "" {
		pipe.ZRem(ctx, q.oldestKey(kind, region), workID)
	}
	if leafID != "" {
		pipe.ZRem(ctx, q.prefix+":ready:"+string(kind)+":leaf:"+leafID, workID)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (q *Queue) pruneEmptyHierarchy(ctx context.Context, message dispatch.Message) error {
	const script = `
if redis.call('ZCARD', KEYS[1]) == 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
  if redis.call('ZCARD', KEYS[2]) == 0 then
    redis.call('ZREM', KEYS[3], ARGV[2])
    if redis.call('ZCARD', KEYS[3]) == 0 then
      redis.call('ZREM', KEYS[4], ARGV[3])
    end
  end
end
return 1`
	keys := []string{q.readyKey(message), q.leavesKey(message.WorkKind, message.RegionID, message.OrgID, message.EnvironmentID),
		q.environmentsKey(message.WorkKind, message.RegionID, message.OrgID), q.organizationsKey(message.WorkKind, message.RegionID)}
	if err := q.client.Eval(ctx, script, keys, q.leafID(message), message.EnvironmentID, message.OrgID).Err(); err != nil {
		return fmt.Errorf("%w: prune ready hierarchy: %v", dispatch.ErrQueueUnavailable, err)
	}
	return nil
}

func sanitizeKeyPart(value string) string {
	if value == "" {
		return "_"
	}
	// Region IDs are DB-valid arbitrary nonblank text, so replacement-based
	// escaping is not injective (for example, a:b and a_b collided). Raw URL
	// base64 is delimiter-safe and preserves every byte of the original value.
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func readyScore(priority int32, timestamp time.Time) float64 {
	return float64(-priority)*1e15 + float64(timestamp.UTC().UnixMilli())
}
