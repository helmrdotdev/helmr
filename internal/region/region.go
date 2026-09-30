// Package region owns regions: the region ID grammar and the operations on the
// regions table. Operations take domain inputs and return the errors declared
// here; callers map them to their transport.
package region

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var (
	ErrNotFound = errors.New("region not found")
	ErrExists   = errors.New("region identity is already in use")
)

// InputError reports a caller-supplied value that the region domain rejects.
type InputError struct {
	message string
}

func (e InputError) Error() string {
	return e.message
}

// Details are the fields of a region. DisplayName and Location are trimmed
// before use.
type Details struct {
	ID          string
	DisplayName string
	Location    string
}

// Patch replaces the fields that are set.
type Patch struct {
	DisplayName *string
	Location    *string
}

// List returns every region.
func List(ctx context.Context, q db.Querier) ([]db.Region, error) {
	regions, err := q.ListRegions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	return regions, nil
}

// Get returns one region.
func Get(ctx context.Context, q db.Querier, id string) (db.Region, error) {
	found, err := q.GetRegion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Region{}, ErrNotFound
	}
	if err != nil {
		return db.Region{}, fmt.Errorf("get region: %w", err)
	}
	return found, nil
}

// Create adds a region. The display name is required.
func Create(ctx context.Context, q db.Querier, details Details) (db.Region, error) {
	details.DisplayName = strings.TrimSpace(details.DisplayName)
	details.Location = strings.TrimSpace(details.Location)
	if err := ValidateID(details.ID); err != nil {
		return db.Region{}, InputError{message: err.Error()}
	}
	if details.DisplayName == "" {
		return db.Region{}, InputError{message: "display_name is required"}
	}
	created, err := q.CreateRegion(ctx, db.CreateRegionParams{
		ID: details.ID, DisplayName: details.DisplayName, Location: details.Location,
	})
	if db.IsUniqueViolation(err) {
		return db.Region{}, ErrExists
	}
	if err != nil {
		return db.Region{}, fmt.Errorf("create region: %w", err)
	}
	return created, nil
}

// Update applies patch to a region's metadata in one statement, so concurrent
// patches of different fields both take effect. An absent field is kept; a set
// field replaces the stored value after trimming.
func Update(ctx context.Context, q db.Querier, id string, patch Patch) (db.Region, error) {
	params := db.UpdateRegionMetadataParams{ID: id}
	if patch.DisplayName != nil {
		params.SetDisplayName = true
		params.DisplayName = strings.TrimSpace(*patch.DisplayName)
		if params.DisplayName == "" {
			return db.Region{}, InputError{message: "display_name is required"}
		}
	}
	if patch.Location != nil {
		params.SetLocation = true
		params.Location = strings.TrimSpace(*patch.Location)
	}
	updated, err := q.UpdateRegionMetadata(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Region{}, ErrNotFound
	}
	if err != nil {
		return db.Region{}, fmt.Errorf("update region: %w", err)
	}
	return updated, nil
}

// Ensure creates the region when it does not exist and leaves an existing
// region unchanged, including when another caller creates it concurrently. An
// empty display name takes the ID.
func Ensure(ctx context.Context, q db.Querier, details Details) error {
	details.DisplayName = strings.TrimSpace(details.DisplayName)
	details.Location = strings.TrimSpace(details.Location)
	if err := ValidateID(details.ID); err != nil {
		return InputError{message: err.Error()}
	}
	if details.DisplayName == "" {
		details.DisplayName = details.ID
	}
	if err := q.EnsureRegion(ctx, db.EnsureRegionParams{
		ID: details.ID, DisplayName: details.DisplayName, Location: details.Location,
	}); err != nil {
		return fmt.Errorf("ensure region: %w", err)
	}
	return nil
}
