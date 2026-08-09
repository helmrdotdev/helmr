package dispatch

import (
	"bytes"
	"sort"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type runPlacementCandidateCursor struct {
	score        pgtype.Timestamptz
	runID        pgtype.UUID
	set          bool
	exhausted    bool
	pendingUntil time.Time
}

type runPlacementOrganizationCursor struct {
	after      runPlacementScopeCursor
	seen       map[runPlacementScope]struct{}
	candidates map[runPlacementScope]runPlacementCandidateCursor
}

type runPlacementCursor struct {
	afterOrganization pgtype.UUID
	afterPending      runPlacementScope
	afterPendingSet   bool
	seen              map[pgtype.UUID]struct{}
	organizations     map[pgtype.UUID]*runPlacementOrganizationCursor
}

func (c *runPlacementCursor) chooseOrganizations(rows []pgtype.UUID, limit int) []pgtype.UUID {
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

func (c *runPlacementCursor) scopeParams(
	organizations []pgtype.UUID,
	limit int32,
) runPlacementScopeParams {
	c.init()
	after := make([]runPlacementScopeCursor, 0, len(organizations))
	for _, organizationID := range organizations {
		after = append(after, c.organization(organizationID).after)
	}
	return runPlacementScopeParams{organizations: organizations, after: after, limit: limit}
}

func (c *runPlacementCursor) chooseScopes(
	rows []runPlacementScopeRow,
	organizations []pgtype.UUID,
	limit int,
	fetchLimit int,
) ([]runPlacementScope, map[pgtype.UUID]bool) {
	c.init()
	returned := make(map[pgtype.UUID]int, len(organizations))
	for _, row := range rows {
		returned[row.scope.orgID]++
	}
	count := min(len(rows), limit)
	selected := make([]runPlacementScope, 0, count)
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

func (c *runPlacementCursor) advanceScopes(
	organizationID pgtype.UUID,
	scopes []runPlacementScopeCandidates,
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
	state.after = runPlacementScopeCursor{
		environmentID: scope.environmentID,
		queueName:     scope.queueName, concurrencyKey: scope.concurrencyKey, set: true,
	}
}

func (c *runPlacementOrganizationCursor) finishScopePass() {
	for scope := range c.candidates {
		if _, ok := c.seen[scope]; !ok {
			delete(c.candidates, scope)
		}
	}
	clear(c.seen)
	c.after = runPlacementScopeCursor{}
}

func (c *runPlacementCursor) candidateParams(
	scopes []runPlacementScope,
	limits []int32,
) db.ListQueuedRunPlacementCandidatesParams {
	c.init()
	params := db.ListQueuedRunPlacementCandidatesParams{
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

func (c *runPlacementCursor) beginCycle() {
	c.init()
	for _, organization := range c.organizations {
		for scope, candidate := range organization.candidates {
			candidate.exhausted = false
			organization.candidates[scope] = candidate
		}
	}
}

func (c *runPlacementCursor) readyCandidateScopes(
	scopes []runPlacementScope,
) []runPlacementScope {
	ready := make([]runPlacementScope, 0, len(scopes))
	for _, scope := range scopes {
		candidate := c.organization(scope.orgID).candidates[scope]
		if candidate.exhausted || !candidate.pendingUntil.IsZero() {
			continue
		}
		ready = append(ready, scope)
	}
	return ready
}

func (c *runPlacementCursor) duePendingScopes(now time.Time, limit int) []runPlacementScope {
	c.init()
	if limit <= 0 {
		return nil
	}
	var due []runPlacementScope
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
		return compareRunPlacementScopes(due[i], due[j]) < 0
	})
	start := 0
	if c.afterPendingSet {
		start = sort.Search(len(due), func(i int) bool {
			return compareRunPlacementScopes(due[i], c.afterPending) > 0
		})
		if start == len(due) {
			start = 0
		}
	}
	count := min(limit, len(due))
	selected := make([]runPlacementScope, 0, count)
	for offset := range count {
		selected = append(selected, due[(start+offset)%len(due)])
	}
	c.afterPending = selected[len(selected)-1]
	c.afterPendingSet = true
	return selected
}

func compareRunPlacementScopes(left, right runPlacementScope) int {
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

func (c *runPlacementCursor) advanceCandidate(
	scope runPlacementScope,
	row db.ListQueuedRunPlacementCandidatesRow,
	end bool,
) {
	state := c.organization(scope.orgID)
	if end {
		state.candidates[scope] = runPlacementCandidateCursor{exhausted: true}
		return
	}
	state.candidates[scope] = runPlacementCandidateCursor{
		score: row.QueueScoreAt, runID: row.RunID, set: true,
	}
}

func (c *runPlacementCursor) deferCandidate(scope runPlacementScope, until time.Time) {
	state := c.organization(scope.orgID)
	candidate := state.candidates[scope]
	candidate.pendingUntil = until
	state.candidates[scope] = candidate
}

func (c *runPlacementCursor) resetCandidate(scope runPlacementScope) {
	c.organization(scope.orgID).candidates[scope] = runPlacementCandidateCursor{exhausted: true}
}

func (c *runPlacementCursor) organization(
	organizationID pgtype.UUID,
) *runPlacementOrganizationCursor {
	c.init()
	state := c.organizations[organizationID]
	if state == nil {
		state = &runPlacementOrganizationCursor{
			seen:       make(map[runPlacementScope]struct{}),
			candidates: make(map[runPlacementScope]runPlacementCandidateCursor),
		}
		c.organizations[organizationID] = state
	}
	return state
}

func (c *runPlacementCursor) init() {
	if c.seen == nil {
		c.seen = make(map[pgtype.UUID]struct{})
	}
	if c.organizations == nil {
		c.organizations = make(map[pgtype.UUID]*runPlacementOrganizationCursor)
	}
}
