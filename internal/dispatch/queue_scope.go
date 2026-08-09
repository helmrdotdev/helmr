package dispatch

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type QueueScope struct {
	OrgID          pgtype.UUID
	RegionID       string
	ProjectID      pgtype.UUID
	EnvironmentID  pgtype.UUID
	ConcurrencyKey string
	QueueName      string
}

type QueueScopeSelector interface {
	Order([]QueueScope) []QueueScope
}

type RoundRobinQueueScopeSelector struct{}

func runCandidateParams(scopes []QueueScope, limit int32) (db.ListQueuedRunDispatchCandidatesForScopesParams, error) {
	if len(scopes) == 0 || len(scopes) > 32 {
		return db.ListQueuedRunDispatchCandidatesForScopesParams{}, fmt.Errorf("run candidate scope count must be between 1 and 32: %d", len(scopes))
	}
	p := db.ListQueuedRunDispatchCandidatesForScopesParams{
		PerScopeLimit: limit,
		OrgIds:        make([]pgtype.UUID, 0, len(scopes)), ProjectIds: make([]pgtype.UUID, 0, len(scopes)),
		EnvironmentIds: make([]pgtype.UUID, 0, len(scopes)), RegionIds: make([]string, 0, len(scopes)),
		ConcurrencyKeys: make([]string, 0, len(scopes)), QueueNames: make([]string, 0, len(scopes)),
	}
	for _, scope := range scopes {
		p.OrgIds = append(p.OrgIds, scope.OrgID)
		p.ProjectIds = append(p.ProjectIds, scope.ProjectID)
		p.EnvironmentIds = append(p.EnvironmentIds, scope.EnvironmentID)
		p.RegionIds = append(p.RegionIds, scope.RegionID)
		p.ConcurrencyKeys = append(p.ConcurrencyKeys, scope.ConcurrencyKey)
		p.QueueNames = append(p.QueueNames, scope.QueueName)
	}
	return p, nil
}

func (RoundRobinQueueScopeSelector) Order(scopes []QueueScope) []QueueScope {
	if len(scopes) <= 1 {
		return scopes
	}
	orgOrder := make([]pgtype.UUID, 0)
	grouped := make(map[pgtype.UUID][]QueueScope)
	for _, scope := range scopes {
		if _, ok := grouped[scope.OrgID]; !ok {
			orgOrder = append(orgOrder, scope.OrgID)
		}
		grouped[scope.OrgID] = append(grouped[scope.OrgID], scope)
	}
	ordered := make([]QueueScope, 0, len(scopes))
	for index := 0; len(ordered) < len(scopes); index++ {
		for _, orgID := range orgOrder {
			group := grouped[orgID]
			if index < len(group) {
				ordered = append(ordered, group[index])
			}
		}
	}
	return ordered
}
