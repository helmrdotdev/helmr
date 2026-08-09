package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const listRunPlacementOrganizationsSQL = `
WITH RECURSIVE organization_heads AS (
    (
        SELECT runs.org_id,
               1::bigint AS position
          FROM runs
         WHERE runs.status = 'queued'
           AND runs.current_run_lease_id IS NULL
           AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
           AND (get_byte(uuid_send(runs.org_id), 15) & 63) = $1::smallint
           AND ($2::uuid IS NULL OR runs.org_id > $2::uuid)
         ORDER BY runs.org_id, runs.environment_id, runs.queue_name,
                  coalesce(runs.concurrency_key, ''), runs.queue_score_at, runs.id
         LIMIT 1
    )
    UNION ALL
    SELECT next_organization.org_id,
           organization_heads.position + 1
      FROM organization_heads
      CROSS JOIN LATERAL (
          SELECT runs.org_id
            FROM runs
           WHERE runs.status = 'queued'
             AND runs.current_run_lease_id IS NULL
             AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
             AND (get_byte(uuid_send(runs.org_id), 15) & 63) = $1::smallint
             AND runs.org_id > organization_heads.org_id
           ORDER BY runs.org_id, runs.environment_id, runs.queue_name,
                    coalesce(runs.concurrency_key, ''), runs.queue_score_at, runs.id
           LIMIT 1
      ) AS next_organization
     WHERE organization_heads.position < $3
)
SELECT org_id
  FROM organization_heads
 ORDER BY position`

const listRunPlacementScopesSQL = `
WITH RECURSIVE input_organizations AS (
    SELECT input_orgs.position::bigint AS organization_ordinal,
           input_orgs.org_id,
           input_after_set.after_set,
           input_after_environments.environment_id AS after_environment_id,
           input_after_queues.queue_name AS after_queue_name,
           input_after_concurrency_keys.concurrency_key AS after_concurrency_key
      FROM unnest($1::uuid[]) WITH ORDINALITY AS input_orgs(org_id, position)
      JOIN unnest($2::boolean[]) WITH ORDINALITY AS input_after_set(after_set, position)
        ON input_after_set.position = input_orgs.position
      JOIN unnest($3::uuid[]) WITH ORDINALITY AS input_after_environments(environment_id, position)
        ON input_after_environments.position = input_orgs.position
      JOIN unnest($4::text[]) WITH ORDINALITY AS input_after_queues(queue_name, position)
        ON input_after_queues.position = input_orgs.position
      JOIN unnest($5::text[]) WITH ORDINALITY AS input_after_concurrency_keys(concurrency_key, position)
        ON input_after_concurrency_keys.position = input_orgs.position
), scope_heads AS (
    SELECT input_organizations.organization_ordinal,
           first_scope.org_id,
           first_scope.environment_id,
           first_scope.queue_name,
           first_scope.concurrency_key,
           1::bigint AS scope_position
      FROM input_organizations
      CROSS JOIN LATERAL (
          SELECT runs.org_id,
                 runs.environment_id,
                 runs.queue_name,
                 coalesce(runs.concurrency_key, '') AS concurrency_key
            FROM runs
           WHERE runs.status = 'queued'
             AND runs.current_run_lease_id IS NULL
             AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
             AND runs.org_id = input_organizations.org_id
             AND (
                 NOT input_organizations.after_set
                 OR (runs.environment_id, runs.queue_name, coalesce(runs.concurrency_key, ''))
                    > (input_organizations.after_environment_id,
                       input_organizations.after_queue_name,
                       input_organizations.after_concurrency_key)
             )
           ORDER BY runs.org_id, runs.environment_id, runs.queue_name,
                    coalesce(runs.concurrency_key, ''), runs.queue_score_at, runs.id
           LIMIT 1
      ) AS first_scope
    UNION ALL
    SELECT scope_heads.organization_ordinal,
           next_scope.org_id,
           next_scope.environment_id,
           next_scope.queue_name,
           next_scope.concurrency_key,
           scope_heads.scope_position + 1
      FROM scope_heads
      CROSS JOIN LATERAL (
          SELECT runs.org_id,
                 runs.environment_id,
                 runs.queue_name,
                 coalesce(runs.concurrency_key, '') AS concurrency_key
            FROM runs
           WHERE runs.status = 'queued'
             AND runs.current_run_lease_id IS NULL
             AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
             AND runs.org_id = scope_heads.org_id
             AND (runs.environment_id, runs.queue_name, coalesce(runs.concurrency_key, ''))
                 > (scope_heads.environment_id, scope_heads.queue_name, scope_heads.concurrency_key)
           ORDER BY runs.org_id, runs.environment_id, runs.queue_name,
                    coalesce(runs.concurrency_key, ''), runs.queue_score_at, runs.id
           LIMIT 1
      ) AS next_scope
     WHERE scope_heads.scope_position < $6
)
SELECT organization_ordinal, org_id, environment_id, queue_name, concurrency_key
  FROM scope_heads
 ORDER BY scope_position, organization_ordinal`

type runPlacementScope struct {
	orgID          pgtype.UUID
	environmentID  pgtype.UUID
	queueName      string
	concurrencyKey string
}

type runPlacementScopeCursor struct {
	environmentID  pgtype.UUID
	queueName      string
	concurrencyKey string
	set            bool
}

type runPlacementScopeRow struct {
	organizationOrdinal int64
	scope               runPlacementScope
}

type runPlacementScopeParams struct {
	organizations []pgtype.UUID
	after         []runPlacementScopeCursor
	limit         int32
}

const (
	runPlacementLaneCount           = 64
	runPlacementCandidateScopeLimit = 32
)

func runPlacementLane(organizationID pgtype.UUID) int16 {
	if !organizationID.Valid {
		return 0
	}
	return runPlacementLaneBytes(organizationID.Bytes)
}

func runPlacementLaneBytes(organizationID [16]byte) int16 {
	return int16(organizationID[15] & byte(runPlacementLaneCount-1))
}

type RunPlacementStore struct {
	db      db.DBTX
	queries *db.Queries
}

func NewRunPlacementStore(database db.DBTX) (*RunPlacementStore, error) {
	if database == nil {
		return nil, errors.New("run placement database is required")
	}
	return &RunPlacementStore{db: database, queries: db.New(database)}, nil
}

func (s *RunPlacementStore) ListOrganizations(
	ctx context.Context,
	lane int16,
	after pgtype.UUID,
	limit int32,
) ([]pgtype.UUID, error) {
	if lane < 0 || lane >= runPlacementLaneCount {
		return nil, errors.New("run placement lane is out of range")
	}
	if limit <= 0 {
		return nil, errors.New("run placement organization limit must be positive")
	}
	rows, err := s.db.Query(ctx, listRunPlacementOrganizationsSQL, lane, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	organizations := make([]pgtype.UUID, 0, limit)
	for rows.Next() {
		var organizationID pgtype.UUID
		if err := rows.Scan(&organizationID); err != nil {
			return nil, err
		}
		organizations = append(organizations, organizationID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return organizations, nil
}

func (s *RunPlacementStore) ListScopes(
	ctx context.Context,
	params runPlacementScopeParams,
) ([]runPlacementScopeRow, error) {
	if len(params.organizations) == 0 || len(params.organizations) != len(params.after) {
		return nil, errors.New("run placement organizations and cursors must be non-empty and aligned")
	}
	afterSet := make([]bool, 0, len(params.after))
	afterEnvironmentIDs := make([]pgtype.UUID, 0, len(params.after))
	afterQueueNames := make([]string, 0, len(params.after))
	afterConcurrencyKeys := make([]string, 0, len(params.after))
	for index, cursor := range params.after {
		afterSet = append(afterSet, cursor.set)
		if cursor.set {
			afterEnvironmentIDs = append(afterEnvironmentIDs, cursor.environmentID)
		} else {
			afterEnvironmentIDs = append(afterEnvironmentIDs, params.organizations[index])
		}
		afterQueueNames = append(afterQueueNames, cursor.queueName)
		afterConcurrencyKeys = append(afterConcurrencyKeys, cursor.concurrencyKey)
	}
	rows, err := s.db.Query(
		ctx,
		listRunPlacementScopesSQL,
		params.organizations,
		afterSet,
		afterEnvironmentIDs,
		afterQueueNames,
		afterConcurrencyKeys,
		params.limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]runPlacementScopeRow, 0)
	for rows.Next() {
		var row runPlacementScopeRow
		if err := rows.Scan(
			&row.organizationOrdinal,
			&row.scope.orgID,
			&row.scope.environmentID,
			&row.scope.queueName,
			&row.scope.concurrencyKey,
		); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *RunPlacementStore) ListCandidates(
	ctx context.Context,
	params db.ListQueuedRunPlacementCandidatesParams,
) ([]db.ListQueuedRunPlacementCandidatesRow, error) {
	count := len(params.OrgIds)
	if count == 0 || count > runPlacementCandidateScopeLimit {
		return nil, errors.New("run placement candidate scope count is out of range")
	}
	if len(params.EnvironmentIds) != count ||
		len(params.ConcurrencyKeys) != count ||
		len(params.QueueNames) != count ||
		len(params.CandidateLimits) != count ||
		len(params.AfterSet) != count ||
		len(params.AfterQueueScoreAt) != count ||
		len(params.AfterRunIds) != count {
		return nil, errors.New("run placement candidate scope inputs must be aligned")
	}
	for _, limit := range params.CandidateLimits {
		if limit <= 0 || limit > runPlacementCandidateScopeLimit {
			return nil, errors.New("run placement candidate limit is out of range")
		}
	}
	return s.queries.ListQueuedRunPlacementCandidates(ctx, params)
}
