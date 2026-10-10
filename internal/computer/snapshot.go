package computer

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

// Scope addresses the Computers of one environment.
type Scope struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
}

// Status is the public lifecycle of a Computer.
type Status string

const (
	StatusAvailable Status = "available"
	StatusDeleted   Status = "deleted"
	StatusDeleting  Status = "deleting"
)

// Residency describes where a Computer's execution state currently lives.
type Residency string

// Snapshot is the public projection of one Computer. Its JSON encoding is the
// retained creation receipt for the platform operation.
type Snapshot struct {
	Residency      Residency                 `json:"residency"`
	Error          json.RawMessage           `json:"error,omitempty"`
	ID             string                    `json:"id"`
	Key            *string                   `json:"key,omitempty"`
	DefinitionKey  string                    `json:"definition_key"`
	DeploymentID   string                    `json:"deployment_id"`
	Status         Status                    `json:"status"`
	Secrets        []secretbinding.Reference `json:"secrets"`
	LastActivityAt time.Time                 `json:"last_activity_at"`
	CreatedAt      time.Time                 `json:"created_at"`
	UpdatedAt      time.Time                 `json:"updated_at"`
}

// ListItem is the projection of one Computer in a list.
type ListItem struct {
	Residency      Residency
	Error          json.RawMessage
	ID             string
	Key            *string
	DefinitionKey  string
	DeploymentID   string
	Status         Status
	LastActivityAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ListPosition is the last item of a previous list page.
type ListPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListPage selects up to Limit Computers after an optional position.
type ListPage struct {
	Limit int32
	After *ListPosition
}

// Listing is one page of Computers; More reports that a later page exists.
type Listing struct {
	Items []ListItem
	More  bool
}

const computerProjection = `SELECT c.id::text,c.key,COALESCE(c.origin_definition_key,''),COALESCE(c.origin_deployment_id::text,''),
 CASE WHEN c.deleted_at IS NULL THEN 'available' WHEN l.epoch IS NOT NULL OR c.storage_reservation_bytes IS NOT NULL THEN 'deleting' ELSE 'deleted' END,
 CASE WHEN c.integrity_fault_at IS NOT NULL OR c.preparation_failed_at IS NOT NULL OR l.status='lost' THEN 'unavailable'
 WHEN cp.status='restoring' THEN 'restoring' WHEN l.status='releasing' THEN 'parking'
 WHEN l.status='active' THEN 'running' WHEN l.status='acquiring' OR c.initial_root_id IS NULL THEN 'starting' ELSE 'cold' END,
 CASE WHEN c.integrity_fault_at IS NOT NULL THEN jsonb_build_object('code','computer_recovery_required','message',c.integrity_fault_reason)
 WHEN c.preparation_failed_at IS NOT NULL THEN jsonb_build_object('code','computer_preparation_failed','message','Computer preparation failed')
 WHEN l.status='lost' THEN jsonb_build_object('code','computer_recovery_required','message','Computer execution state is unavailable') END,
 c.last_activity_at,c.created_at,c.updated_at
 FROM computers c JOIN environments e ON e.id=c.environment_id
 LEFT JOIN computer_leases l ON l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL
 LEFT JOIN computer_checkpoints cp ON cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status IN ('capturing','sealed','ready','restoring','aborting')
 WHERE e.org_id=$1 AND e.project_id=$2 AND c.environment_id=$3`

func scanComputer(row pgx.Row) (ListItem, error) {
	var item ListItem
	err := row.Scan(&item.ID, &item.Key, &item.DefinitionKey, &item.DeploymentID, &item.Status, &item.Residency, &item.Error, &item.LastActivityAt, &item.CreatedAt, &item.UpdatedAt)
	item.LastActivityAt, item.CreatedAt, item.UpdatedAt = item.LastActivityAt.UTC(), item.CreatedAt.UTC(), item.UpdatedAt.UTC()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return item, err
}
func Read(ctx context.Context, database db.DBTX, scope Scope, id uuid.UUID) (Snapshot, error) {
	item, err := scanComputer(database.QueryRow(ctx, computerProjection+` AND c.id=$4`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, id))
	if err != nil {
		return Snapshot{}, err
	}
	rows, err := database.Query(ctx, `SELECT secret_id::text,placement_kind,placement_target,mode,allowed_origins FROM computer_secret_bindings WHERE environment_id=$1 AND computer_id=$2 ORDER BY placement_kind,placement_target`, scope.EnvironmentID, id)
	if err != nil {
		return Snapshot{}, err
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (secretbinding.Reference, error) {
		var r secretbinding.Reference
		var kind, target, mode string
		var origins []string
		err := row.Scan(&r.SecretID, &kind, &target, &mode, &origins)
		if kind == "env" {
			r.Env = &secretbinding.ReferenceEnv{Name: target, Mode: mode, AllowedOrigins: origins}
		} else {
			r.File = &secretbinding.File{Path: target}
		}
		return r, err
	})
	if err != nil {
		return Snapshot{}, err
	}
	if refs == nil {
		refs = []secretbinding.Reference{}
	}
	return Snapshot{ID: item.ID, Key: item.Key, DefinitionKey: item.DefinitionKey, DeploymentID: item.DeploymentID, Status: item.Status, Residency: item.Residency, Error: item.Error, Secrets: refs, LastActivityAt: item.LastActivityAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}, nil
}
func FindByKey(ctx context.Context, database db.DBTX, scope Scope, key string) (ListItem, error) {
	return scanComputer(database.QueryRow(ctx, computerProjection+` AND c.key=$4`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, key))
}
func List(ctx context.Context, database db.DBTX, scope Scope, page ListPage) (Listing, error) {
	if page.Limit < 1 || page.Limit > 100 {
		return Listing{}, invalidInput("Computer page limit must be between 1 and 100")
	}
	var afterTime time.Time
	var afterID uuid.UUID
	if page.After != nil {
		afterTime, afterID = page.After.CreatedAt, page.After.ID
	}
	rows, err := database.Query(ctx, computerProjection+` AND (NOT $4 OR (c.created_at,c.id)<($5,$6)) ORDER BY c.created_at DESC,c.id DESC LIMIT $7`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, page.After != nil, afterTime, afterID, page.Limit+1)
	if err != nil {
		return Listing{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ListItem, error) { return scanComputer(row) })
	if err != nil {
		return Listing{}, err
	}
	if items == nil {
		items = []ListItem{}
	}
	result := Listing{Items: items, More: len(items) > int(page.Limit)}
	if result.More {
		result.Items = result.Items[:page.Limit]
	}
	return result, nil
}
