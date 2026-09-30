package dispatch

import (
	"bytes"
	"sort"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type runDispatchCandidateCursor struct {
	score        pgtype.Timestamptz
	runID        pgtype.UUID
	set          bool
	exhausted    bool
	pendingUntil time.Time
}

type runDispatchOrganizationCursor struct {
	after      runDispatchScopeCursor
	seen       map[runDispatchScope]struct{}
	candidates map[runDispatchScope]runDispatchCandidateCursor
}

type runDispatchCursor struct {
	afterOrganization pgtype.UUID
	afterPending      runDispatchScope
	afterPendingSet   bool
	seen              map[pgtype.UUID]struct{}
	organizations     map[pgtype.UUID]*runDispatchOrganizationCursor
}

func (c *runDispatchCursor) chooseOrganizations(rows []pgtype.UUID, limit int) []pgtype.UUID {
	c.init()
	count := min(len(rows), limit)
	selected := rows[:count]
	for _, organizationID := range selected {
		c.seen[organizationID] = struct{}{}
		c.organization(organizationID)
	}
	if len(rows) <= limit {
		for organizationID := range c.organizations {
			if _, ok := c.seen[organizationID]; !ok {
				delete(c.organizations, organizationID)
			}
		}
		clear(c.seen)
		c.afterOrganization = pgtype.UUID{}
	} else {
		c.afterOrganization = selected[len(selected)-1]
	}
	return selected
}

func (c *runDispatchCursor) scopeParams(
	organizations []pgtype.UUID,
	limit int32,
) runDispatchScopeParams {
	c.init()
	after := make([]runDispatchScopeCursor, 0, len(organizations))
	for _, organizationID := range organizations {
		after = append(after, c.organization(organizationID).after)
	}
	return runDispatchScopeParams{organizations: organizations, after: after, limit: limit}
}

func (c *runDispatchCursor) chooseScopes(
	rows []runDispatchScopeRow,
	organizations []pgtype.UUID,
	limit int,
	fetchLimit int,
) ([]runDispatchScope, map[pgtype.UUID]bool) {
	c.init()
	returned := make(map[pgtype.UUID]int, len(organizations))
	for _, row := range rows {
		returned[row.scope.orgID]++
	}
	count := min(len(rows), limit)
	selected := make([]runDispatchScope, 0, count)
	selectedByOrganization := make(map[pgtype.UUID]int, len(organizations))
	for _, row := range rows[:count] {
		selected = append(selected, row.scope)
		selectedByOrganization[row.scope.orgID]++
	}
	ends := make(map[pgtype.UUID]bool, len(organizations))
	for _, organizationID := range organizations {
		state := c.organization(organizationID)
		selectedCount := selectedByOrganization[organizationID]
		if returned[organizationID] == 0 {
			state.finishScopePass()
			continue
		}
		ends[organizationID] = selectedCount == returned[organizationID] &&
			returned[organizationID] < fetchLimit
	}
	return selected, ends
}

func (c *runDispatchCursor) advanceScopes(
	organizationID pgtype.UUID,
	scopes []runDispatchScopeCandidates,
	examined int,
	end bool,
) {
	if examined == 0 {
		return
	}
	state := c.organization(organizationID)
	for _, scope := range scopes[:examined] {
		state.seen[scope.scope] = struct{}{}
	}
	if end && examined == len(scopes) {
		state.finishScopePass()
		return
	}
	scope := scopes[examined-1].scope
	state.after = runDispatchScopeCursor{
		environmentID: scope.environmentID,
		queueName:     scope.queueName, concurrencyKey: scope.concurrencyKey, set: true,
	}
}

func (c *runDispatchOrganizationCursor) finishScopePass() {
	for scope := range c.candidates {
		if _, ok := c.seen[scope]; !ok {
			delete(c.candidates, scope)
		}
	}
	clear(c.seen)
	c.after = runDispatchScopeCursor{}
}

func (c *runDispatchCursor) candidateParams(
	scopes []runDispatchScope,
	limits []int32,
) db.ListQueuedRunDispatchCandidatesParams {
	c.init()
	params := db.ListQueuedRunDispatchCandidatesParams{
		CandidateLimits: limits,
		OrgIds:          make([]pgtype.UUID, 0, len(scopes)), EnvironmentIds: make([]pgtype.UUID, 0, len(scopes)),
		ConcurrencyKeys: make([]string, 0, len(scopes)), QueueNames: make([]string, 0, len(scopes)),
		AfterSet: make([]bool, 0, len(scopes)), AfterQueueScoreAt: make([]pgtype.Timestamptz, 0, len(scopes)),
		AfterRunIds: make([]pgtype.UUID, 0, len(scopes)),
	}
	for _, scope := range scopes {
		cursor := c.organization(scope.orgID).candidates[scope]
		params.OrgIds = append(params.OrgIds, scope.orgID)
		params.EnvironmentIds = append(params.EnvironmentIds, scope.environmentID)
		params.ConcurrencyKeys = append(params.ConcurrencyKeys, scope.concurrencyKey)
		params.QueueNames = append(params.QueueNames, scope.queueName)
		params.AfterSet = append(params.AfterSet, cursor.set)
		params.AfterQueueScoreAt = append(params.AfterQueueScoreAt, cursor.score)
		params.AfterRunIds = append(params.AfterRunIds, cursor.runID)
	}
	return params
}

func (c *runDispatchCursor) beginCycle() {
	c.init()
	for _, organization := range c.organizations {
		for scope, candidate := range organization.candidates {
			candidate.exhausted = false
			organization.candidates[scope] = candidate
		}
	}
}

func (c *runDispatchCursor) readyCandidateScopes(
	scopes []runDispatchScope,
) []runDispatchScope {
	ready := make([]runDispatchScope, 0, len(scopes))
	for _, scope := range scopes {
		candidate := c.organization(scope.orgID).candidates[scope]
		if candidate.exhausted || !candidate.pendingUntil.IsZero() {
			continue
		}
		ready = append(ready, scope)
	}
	return ready
}

func (c *runDispatchCursor) duePendingScopes(now time.Time, limit int) []runDispatchScope {
	c.init()
	if limit <= 0 {
		return nil
	}
	var due []runDispatchScope
	for _, organization := range c.organizations {
		for scope, candidate := range organization.candidates {
			if candidate.pendingUntil.IsZero() || candidate.pendingUntil.After(now) {
				continue
			}
			due = append(due, scope)
		}
	}
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool {
		return compareRunDispatchScopes(due[i], due[j]) < 0
	})
	start := 0
	if c.afterPendingSet {
		start = sort.Search(len(due), func(i int) bool {
			return compareRunDispatchScopes(due[i], c.afterPending) > 0
		})
		if start == len(due) {
			start = 0
		}
	}
	count := min(limit, len(due))
	selected := make([]runDispatchScope, 0, count)
	for offset := range count {
		selected = append(selected, due[(start+offset)%len(due)])
	}
	c.afterPending = selected[len(selected)-1]
	c.afterPendingSet = true
	return selected
}

func compareRunDispatchScopes(left, right runDispatchScope) int {
	if compared := bytes.Compare(left.orgID.Bytes[:], right.orgID.Bytes[:]); compared != 0 {
		return compared
	}
	if compared := bytes.Compare(left.environmentID.Bytes[:], right.environmentID.Bytes[:]); compared != 0 {
		return compared
	}
	if left.queueName < right.queueName {
		return -1
	}
	if left.queueName > right.queueName {
		return 1
	}
	if left.concurrencyKey < right.concurrencyKey {
		return -1
	}
	if left.concurrencyKey > right.concurrencyKey {
		return 1
	}
	return 0
}

func (c *runDispatchCursor) advanceCandidate(
	scope runDispatchScope,
	row db.ListQueuedRunDispatchCandidatesRow,
	end bool,
) {
	state := c.organization(scope.orgID)
	if end {
		state.candidates[scope] = runDispatchCandidateCursor{exhausted: true}
		return
	}
	state.candidates[scope] = runDispatchCandidateCursor{
		score: row.QueueScoreAt, runID: row.RunID, set: true,
	}
}

func (c *runDispatchCursor) deferCandidate(scope runDispatchScope, until time.Time) {
	state := c.organization(scope.orgID)
	candidate := state.candidates[scope]
	candidate.pendingUntil = until
	state.candidates[scope] = candidate
}

func (c *runDispatchCursor) resetCandidate(scope runDispatchScope) {
	c.organization(scope.orgID).candidates[scope] = runDispatchCandidateCursor{exhausted: true}
}

func (c *runDispatchCursor) organization(
	organizationID pgtype.UUID,
) *runDispatchOrganizationCursor {
	c.init()
	state := c.organizations[organizationID]
	if state == nil {
		state = &runDispatchOrganizationCursor{
			seen:       make(map[runDispatchScope]struct{}),
			candidates: make(map[runDispatchScope]runDispatchCandidateCursor),
		}
		c.organizations[organizationID] = state
	}
	return state
}

func (c *runDispatchCursor) init() {
	if c.seen == nil {
		c.seen = make(map[pgtype.UUID]struct{})
	}
	if c.organizations == nil {
		c.organizations = make(map[pgtype.UUID]*runDispatchOrganizationCursor)
	}
}
