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

// ExecutionWait describes an observed wait for accepted runnable work.
// It is advisory and does not reserve capacity or promise guest readiness.
type ExecutionWait struct {
	Reason string `json:"reason"`
}

// Snapshot is the public projection of one Computer. Its JSON encoding is the
// retained creation receipt for the platform operation.
type Snapshot struct {
	ExecutionControl    *ExecutionControl         `json:"execution_control"`
	ExecutionWait       *ExecutionWait            `json:"execution_wait"`
	DeleteAfterUnusedMs *int64                    `json:"delete_after_unused_ms"`
	UnusedSince         *time.Time                `json:"unused_since"`
	Residency           Residency                 `json:"residency"`
	Error               json.RawMessage           `json:"error,omitempty"`
	ID                  string                    `json:"id"`
	Key                 *string                   `json:"key,omitempty"`
	DefinitionKey       string                    `json:"template"`
	DeploymentID        string                    `json:"deployment_id"`
	Status              Status                    `json:"status"`
	Secrets             []secretbinding.Reference `json:"secrets"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
}

// ListItem is the projection of one Computer in a list.
type ListItem struct {
	ExecutionControl    *ExecutionControl
	ExecutionWait       *ExecutionWait
	waitCandidate       string
	waitCPU, waitMemory int64
	DeleteAfterUnusedMs *int64
	UnusedSince         *time.Time
	Residency           Residency
	Error               json.RawMessage
	ID                  string
	Key                 *string
	DefinitionKey       string
	DeploymentID        string
	Status              Status
	CreatedAt           time.Time
	UpdatedAt           time.Time
	// SecretExposure is set only when listing by Secret.
	SecretExposure SecretExposure
}

// SecretExposure says how a listed Computer came to hold a raw Secret value.
type SecretExposure string

const (
	// SecretExposureReceived means a process or Command on the Computer
	// received the raw value directly.
	SecretExposureReceived SecretExposure = "received"
	// SecretExposureImage means the Computer's image was prepared with the raw
	// value, so the Computer may hold it.
	SecretExposureImage SecretExposure = "image"
)

// ListPosition is the last item of a previous list page.
type ListPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListPage selects up to Limit Computers after an optional position. A
// non-empty Secret lists only non-deleted Computers that received a raw value
// of that Secret directly or through their image.
type ListPage struct {
	Limit  int32
	After  *ListPosition
	Secret string
}

// secretExposedComputersSQL relates Computers to raw deliveries of the Secret
// named $9 in Environment $3. Session and Command exposures are recorded only
// for raw bindings; image preparation records every binding, so image lineage
// keeps only raw ones.
const secretExposedComputersSQL = `WITH target AS (SELECT id FROM secrets WHERE environment_id=$3 AND name=$9),
exposed AS (
 SELECT p.computer_id,true AS direct FROM target t JOIN secret_exposures x ON x.environment_id=$3 AND x.secret_id=t.id
 JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(x.environment_id,x.session_id,x.process_epoch)
 UNION
 SELECT cmd.computer_id,true FROM target t JOIN secret_exposures x ON x.environment_id=$3 AND x.secret_id=t.id
 JOIN commands cmd ON (cmd.environment_id,cmd.id)=(x.environment_id,x.command_id)
 UNION
 SELECT held.id,false FROM target t JOIN secret_exposures x ON x.environment_id=$3 AND x.secret_id=t.id
 JOIN images i ON (i.environment_id,i.image_build_id)=(x.environment_id,x.image_build_id)
 JOIN computers held ON (held.environment_id,held.image_id)=(i.environment_id,i.id)
 WHERE EXISTS(SELECT 1 FROM secret_bindings b WHERE (b.environment_id,b.image_spec_id,b.secret_id)=(i.environment_id,i.image_spec_id,x.secret_id) AND b.mode='raw')
) `

// Listing is one page of Computers; More reports that a later page exists.
type Listing struct {
	Items []ListItem
	More  bool
}

const computerProjection = `SELECT c.id::text,c.key,(SELECT key FROM templates t WHERE (t.environment_id,t.id)=(c.environment_id,c.origin_template_id)),COALESCE(c.origin_deployment_id::text,''),
 CASE WHEN c.deleted_at IS NULL THEN 'available' WHEN l.epoch IS NOT NULL OR c.storage_reservation_bytes IS NOT NULL THEN 'deleting' ELSE 'deleted' END,
 CASE WHEN c.integrity_fault_at IS NOT NULL OR c.preparation_failed_at IS NOT NULL OR l.status='lost' THEN 'unavailable'
 WHEN cp.status='restoring' THEN 'restoring' WHEN cp.status='ready' THEN 'parked' WHEN cp.status IN ('capturing','sealed') OR l.status='releasing' THEN 'parking'
 WHEN l.status='active' THEN 'running' WHEN l.status='acquiring' OR (c.initial_root_id IS NULL AND c.deleted_at IS NULL) THEN 'starting' ELSE 'cold' END,
 CASE WHEN c.integrity_fault_at IS NOT NULL THEN jsonb_build_object('code','computer_recovery_required','message','Computer requires recovery')
 WHEN c.preparation_failed_at IS NOT NULL THEN jsonb_build_object('code','computer_preparation_failed','message','Computer preparation failed')
 WHEN l.status='lost' THEN jsonb_build_object('code','computer_recovery_required','message','Computer execution state is unavailable') END,
 c.created_at,c.updated_at,(SELECT d.delete_after_unused_ms FROM template_definitions d WHERE (d.environment_id,d.template_id,d.deployment_id)=(c.environment_id,c.origin_template_id,c.origin_deployment_id)),c.unused_since,
 CASE WHEN c.deleted_at IS NOT NULL OR c.integrity_fault_at IS NOT NULL OR c.preparation_failed_at IS NOT NULL
 OR c.initial_root_id IS NULL OR c.image_id IS NULL OR l.status IN ('lost','acquiring') OR cp.status='restoring'
 OR NOT (EXISTS(SELECT 1 FROM computer_controls control WHERE control.environment_id=c.environment_id AND control.computer_id=c.id AND control.kind='wake' AND control.status='accepted')
 OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending')
 OR EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing') AND ` + db.NoSessionHoldsSQL + `
 AND (EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')
 OR NOT EXISTS(SELECT 1 FROM computer_leases prior WHERE prior.environment_id=c.environment_id AND prior.computer_id=c.id AND prior.initialized_at IS NOT NULL)))) THEN ''
 WHEN cp.status IN ('capturing','sealed','aborting') OR (cp.status='ready' AND l.epoch IS NOT NULL) OR l.status='releasing'
 OR EXISTS(SELECT 1 FROM checkpoints pending WHERE pending.environment_id=c.environment_id AND pending.computer_id=c.id AND pending.status='consumed' AND pending.controls_reconciled_at IS NULL)
 OR (l.epoch IS NULL AND EXISTS(SELECT 1 FROM computer_saves pending WHERE pending.environment_id=c.environment_id AND pending.computer_id=c.id AND pending.status IN ('requested','captured'))) THEN 'saving'
 WHEN l.epoch IS NOT NULL THEN ''
 WHEN c.recovery_save_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_saves saved WHERE saved.environment_id=c.environment_id AND saved.id=c.recovery_save_id AND saved.status='published' AND saved.root_id IS NOT NULL) THEN ''
 ELSE 'scheduling' END,
 COALESCE(source.reserved_cpu_millis,(c.cpu_millis+999)/1000*1000),COALESCE(source.reserved_memory_bytes,c.memory_bytes),
 (SELECT jsonb_build_object('id',control.id,'computer_id',control.computer_id,'kind',control.kind,'status',control.status,'error_code',control.error_code,'accepted_at',control.accepted_at,'settled_at',control.settled_at) FROM computer_controls control WHERE control.environment_id=c.environment_id AND control.computer_id=c.id ORDER BY control.accepted_at DESC,control.id DESC LIMIT 1)
 FROM computers c JOIN environments e ON e.id=c.environment_id
 LEFT JOIN computer_leases l ON l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL
 LEFT JOIN checkpoints cp ON cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status IN ('capturing','sealed','ready','restoring','aborting')
 LEFT JOIN computer_leases source ON cp.status='ready' AND (source.environment_id,source.computer_id,source.epoch)=(cp.environment_id,cp.computer_id,cp.source_lease_epoch)
 WHERE e.org_id=$1 AND e.project_id=$2 AND c.environment_id=$3`

func scanComputer(row pgx.Row, extra ...any) (ListItem, error) {
	var item ListItem
	err := row.Scan(append([]any{&item.ID, &item.Key, &item.DefinitionKey, &item.DeploymentID, &item.Status, &item.Residency, &item.Error, &item.CreatedAt, &item.UpdatedAt, &item.DeleteAfterUnusedMs, &item.UnusedSince, &item.waitCandidate, &item.waitCPU, &item.waitMemory, &item.ExecutionControl}, extra...)...)
	item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
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
	items := []ListItem{item}
	if err := projectExecutionWait(ctx, database, scope.EnvironmentID, items); err != nil {
		return Snapshot{}, err
	}
	item = items[0]
	rows, err := database.Query(ctx, `SELECT s.name,b.placement_kind,b.placement_target,b.mode,b.allowed_origins FROM secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id) WHERE b.environment_id=$1 AND b.computer_id=$2 ORDER BY b.placement_kind,b.placement_target`, scope.EnvironmentID, id)
	if err != nil {
		return Snapshot{}, err
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (secretbinding.Reference, error) {
		var r secretbinding.Reference
		var kind, target, mode string
		var origins []string
		err := row.Scan(&r.Secret, &kind, &target, &mode, &origins)
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
	return Snapshot{ExecutionControl: item.ExecutionControl, ExecutionWait: item.ExecutionWait, DeleteAfterUnusedMs: item.DeleteAfterUnusedMs, UnusedSince: item.UnusedSince, ID: item.ID, Key: item.Key, DefinitionKey: item.DefinitionKey, DeploymentID: item.DeploymentID, Status: item.Status, Residency: item.Residency, Error: item.Error, Secrets: refs, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}, nil
}
func FindByKey(ctx context.Context, database db.DBTX, scope Scope, key string) (ListItem, error) {
	item, err := scanComputer(database.QueryRow(ctx, computerProjection+` AND c.key=$4`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, key))
	if err != nil {
		return ListItem{}, err
	}
	items := []ListItem{item}
	if err := projectExecutionWait(ctx, database, scope.EnvironmentID, items); err != nil {
		return ListItem{}, err
	}
	return items[0], nil
}
func List(ctx context.Context, database db.DBTX, scope Scope, page ListPage) (Listing, error) {
	return list(ctx, database, scope, page, uuid.Nil())
}

// ListForSession exposes only Computers attached to this workload's own Session,
// descendants and directly requested roots. The caller authorizes the Session.
func ListForSession(ctx context.Context, database db.DBTX, scope Scope, page ListPage, session uuid.UUID) (Listing, error) {
	if session == uuid.Nil() {
		return Listing{}, ErrDenied
	}
	return list(ctx, database, scope, page, session)
}

func list(ctx context.Context, database db.DBTX, scope Scope, page ListPage, session uuid.UUID) (Listing, error) {
	if page.Limit < 1 || page.Limit > 100 {
		return Listing{}, invalidInput("Computer page limit must be between 1 and 100")
	}
	var afterTime time.Time
	var afterID uuid.UUID
	if page.After != nil {
		afterTime, afterID = page.After.CreatedAt, page.After.ID
	}
	var secretName *string
	if page.Secret != "" {
		secretName = &page.Secret
	}
	rows, err := database.Query(ctx, secretExposedComputersSQL+`SELECT page.*,(SELECT bool_or(direct) FROM exposed WHERE exposed.computer_id::text=page.id) FROM (`+computerProjection+` AND ($9::text IS NULL OR (c.deleted_at IS NULL AND c.id IN (SELECT computer_id FROM exposed))) AND ($8::uuid='00000000-0000-0000-0000-000000000000' OR c.id IN (WITH RECURSIVE lineage AS (SELECT id,computer_id FROM sessions WHERE environment_id=$3 AND id=$8 UNION ALL SELECT s.id,s.computer_id FROM sessions s JOIN lineage l ON s.parent_session_id=l.id WHERE s.environment_id=$3) SELECT computer_id FROM lineage UNION SELECT computer_id FROM sessions WHERE environment_id=$3 AND parent_session_id IS NULL AND requester_session_id=$8)) AND (NOT $4 OR (c.created_at,c.id)<($5,$6)) ORDER BY c.created_at DESC,c.id DESC LIMIT $7) page ORDER BY page.created_at DESC,page.id DESC`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, page.After != nil, afterTime, afterID, page.Limit+1, session, secretName)
	if err != nil {
		return Listing{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ListItem, error) {
		var direct *bool
		item, err := scanComputer(row, &direct)
		switch {
		case direct == nil:
		case *direct:
			item.SecretExposure = SecretExposureReceived
		default:
			item.SecretExposure = SecretExposureImage
		}
		return item, err
	})
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
	if err := projectExecutionWait(ctx, database, scope.EnvironmentID, result.Items); err != nil {
		return Listing{}, err
	}
	return result, nil
}

func projectExecutionWait(ctx context.Context, database db.DBTX, env uuid.UUID, items []ListItem) error {
	var budget *db.ExecutionBudget
	for i := range items {
		item := &items[i]
		switch item.waitCandidate {
		case "saving":
			item.ExecutionWait = &ExecutionWait{Reason: "saving"}
		case "scheduling":
			if budget == nil {
				b, err := db.ReadExecutionBudget(ctx, database, env)
				if err != nil {
					return err
				}
				budget = &b
			}
			reason := "scheduling"
			if !budget.Fits(item.waitCPU, item.waitMemory, true) {
				reason = "environment_limit"
			}
			item.ExecutionWait = &ExecutionWait{Reason: reason}
		}
	}
	return nil
}
