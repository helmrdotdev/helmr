package dispatch

import "github.com/jackc/pgx/v5/pgtype"

// QueueScope is the dispatch fairness unit: one queue class inside one worker group and project environment.
type QueueScope struct {
	OrgID         pgtype.UUID
	WorkerGroupID string
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	QueueClass    string
	QueueName     string
}

type QueueScopeSelector interface {
	Order([]QueueScope) []QueueScope
}

type RoundRobinQueueScopeSelector struct{}

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
