package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
// stored creation receipt: field names and tags must stay byte-compatible
// with receipts already written.
type Snapshot struct {
	Residency      Residency               `json:"residency"`
	Error          json.RawMessage         `json:"error,omitempty"`
	ID             string                  `json:"id"`
	Key            *string                 `json:"key,omitempty"`
	SandboxID      string                  `json:"sandbox_id"`
	DeploymentID   string                  `json:"deployment_id"`
	Status         Status                  `json:"status"`
	Secrets        []secretbinding.Binding `json:"secrets"`
	LastActivityAt time.Time               `json:"last_activity_at"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

// ListItem is the projection of one Computer in a list.
type ListItem struct {
	Residency      Residency
	Error          json.RawMessage
	ID             string
	Key            *string
	SandboxID      string
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

// Read returns the Computer's snapshot. q may be a pool or the caller's
// transaction; Read takes no locks.
func Read(ctx context.Context, q db.Querier, scope Scope, id uuid.UUID) (Snapshot, error) {
	record, err := getComputer(ctx, q, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot(ctx, q, record)
}

func getComputer(ctx context.Context, q db.Querier, scope Scope, id uuid.UUID) (db.GetComputerRow, error) {
	record, err := q.GetComputer(ctx, db.GetComputerParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     pgvalue.UUID(scope.ProjectID),
		EnvironmentID: pgvalue.UUID(scope.EnvironmentID),
		ID:            pgvalue.UUID(id),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetComputerRow{}, ErrNotFound
	}
	if err != nil {
		return db.GetComputerRow{}, fmt.Errorf("read computer: %w", err)
	}
	return record, nil
}

func snapshot(ctx context.Context, q db.Querier, record db.GetComputerRow) (Snapshot, error) {
	bindings, err := q.ListComputerSecrets(ctx, record.ID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read computer secrets: %w", err)
	}
	secrets := make([]secretbinding.Binding, 0, len(bindings))
	for _, binding := range bindings {
		item, err := snapshotBinding(binding.SecretName, binding.PlacementKind, binding.PlacementTarget, binding.Mode, binding.AllowedOrigins)
		if err != nil {
			return Snapshot{}, err
		}
		secrets = append(secrets, item)
	}
	status, err := publicStatus(record.Status)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ID:             pgvalue.UUIDString(record.ID),
		Key:            textPointer(record.Key),
		SandboxID:      record.SandboxDeclaredID.String,
		DeploymentID:   pgvalue.UUIDString(record.CreationDeploymentID),
		Status:         status,
		Error:          record.ResidencyError,
		Residency:      Residency(record.Residency),
		Secrets:        secrets,
		LastActivityAt: pgvalue.Time(record.LastActivityAt),
		CreatedAt:      pgvalue.Time(record.CreatedAt),
		UpdatedAt:      pgvalue.Time(record.UpdatedAt),
	}, nil
}

func snapshotBinding(name, kind, target, mode string, origins []string) (secretbinding.Binding, error) {
	item := secretbinding.Binding{Name: name}
	switch kind {
	case "env":
		item.Env = &secretbinding.Env{Name: target, Mode: mode, AllowedOrigins: origins}
	case "file":
		item.File = &secretbinding.File{Path: target}
	default:
		return secretbinding.Binding{}, fmt.Errorf("unsupported computer secret placement %q", kind)
	}
	return item, nil
}

// FindByKey returns the list item of the Computer holding key.
func FindByKey(ctx context.Context, q db.Querier, scope Scope, key string) (ListItem, error) {
	record, err := q.GetComputerListItemByKey(ctx, db.GetComputerListItemByKeyParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     pgvalue.UUID(scope.ProjectID),
		EnvironmentID: pgvalue.UUID(scope.EnvironmentID),
		Key:           pgvalue.Text(key),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ListItem{}, ErrNotFound
	}
	if err != nil {
		return ListItem{}, fmt.Errorf("read computer by key: %w", err)
	}
	item, err := listItem(
		record.ID, record.Key, record.SandboxID, record.DeploymentID, record.Status,
		record.LastActivityAt, record.CreatedAt, record.UpdatedAt,
	)
	if err != nil {
		return ListItem{}, err
	}
	item.Error = record.ResidencyError
	item.Residency = Residency(record.Residency)
	return item, nil
}

// List returns one page of the scope's Computers, newest first.
func List(ctx context.Context, q db.Querier, scope Scope, page ListPage) (Listing, error) {
	params := db.ListComputerListItemsParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     pgvalue.UUID(scope.ProjectID),
		EnvironmentID: pgvalue.UUID(scope.EnvironmentID),
		RowLimit:      page.Limit + 1,
	}
	if page.After != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgtype.Timestamptz{Time: page.After.CreatedAt, Valid: true}
		params.AfterID = pgvalue.UUID(page.After.ID)
	}
	rows, err := q.ListComputerListItems(ctx, params)
	if err != nil {
		return Listing{}, fmt.Errorf("list computers: %w", err)
	}
	listing := Listing{Items: make([]ListItem, 0, len(rows)), More: len(rows) > int(page.Limit)}
	if listing.More {
		rows = rows[:page.Limit]
	}
	for _, row := range rows {
		item, err := listItem(
			row.ID, row.Key, row.SandboxID, row.DeploymentID, row.Status,
			row.LastActivityAt, row.CreatedAt, row.UpdatedAt,
		)
		if err != nil {
			return Listing{}, err
		}
		item.Error = row.ResidencyError
		item.Residency = Residency(row.Residency)
		listing.Items = append(listing.Items, item)
	}
	return listing, nil
}

func listItem(
	id pgtype.UUID,
	key pgtype.Text,
	sandboxID string,
	deploymentID pgtype.UUID,
	state string,
	lastActivityAt, createdAt, updatedAt pgtype.Timestamptz,
) (ListItem, error) {
	status, err := publicStatus(state)
	if err != nil {
		return ListItem{}, err
	}
	return ListItem{
		ID: pgvalue.UUIDString(id), Key: textPointer(key), SandboxID: sandboxID,
		DeploymentID: pgvalue.UUIDString(deploymentID), Status: status,
		LastActivityAt: pgvalue.Time(lastActivityAt), CreatedAt: pgvalue.Time(createdAt),
		UpdatedAt: pgvalue.Time(updatedAt),
	}, nil
}

func publicStatus(state string) (Status, error) {
	switch state {
	case db.ComputerStatusActive, db.ComputerStatusRecoveryRequired:
		return StatusAvailable, nil
	case db.ComputerStatusDeleted:
		return StatusDeleted, nil
	case db.ComputerStatusDeleting:
		return StatusDeleting, nil
	default:
		return "", fmt.Errorf("computer state %q has no public projection", state)
	}
}

func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}
