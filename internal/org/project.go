package org

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ProjectDetails are the editable fields of a project.
type ProjectDetails struct {
	Slug string
	Name string
}

// ProjectInput describes a project to create. An empty DefaultRegionID selects
// the first configured region.
type ProjectInput struct {
	ProjectDetails
	DefaultRegionID  string
	ExecutionLimits  ExecutionLimits
	HistoryRetention HistoryRetentionPolicy
}

// EnvironmentDetails are the editable fields of an environment. ColorHex is
// already normalized to the public #RRGGBB form.
type EnvironmentDetails struct {
	HistoryRetention HistoryRetentionPolicy
	Slug             string
	Name             string
	ColorHex         string
}

// ProjectPosition is the sort key of the last project on a previous page.
type ProjectPosition struct {
	IsDefault bool
	Slug      string
	ID        uuid.UUID
}

// ListProjects returns up to limit projects of the organization after the
// given position, default project first, and whether more follow.
func ListProjects(ctx context.Context, q db.Querier, orgID uuid.UUID, limit int32, after *ProjectPosition) ([]db.Project, bool, error) {
	params := db.ListProjectsParams{OrgID: pgvalue.UUID(orgID), RowLimit: limit + 1}
	if after != nil {
		params.HasAfter = true
		params.AfterIsDefault = after.IsDefault
		params.AfterSlug = after.Slug
		params.AfterID = pgvalue.UUID(after.ID)
	}
	projects, err := q.ListProjects(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list projects: %w", err)
	}
	if len(projects) > int(limit) {
		return projects[:limit], true, nil
	}
	return projects, false, nil
}

// GetProject resolves a public project identifier within the organization.
func GetProject(ctx context.Context, q db.Querier, orgID uuid.UUID, ref string) (db.Project, error) {
	ref = strings.TrimSpace(ref)
	if !slugPattern.MatchString(ref) {
		return db.Project{}, invalidInput("invalid project identifier")
	}
	project, err := q.GetProjectBySlug(ctx, db.GetProjectBySlugParams{OrgID: pgvalue.UUID(orgID), Slug: ref})
	if isNoRows(err) {
		return db.Project{}, ErrProjectNotFound
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("load project: %w", err)
	}
	return project, nil
}

// ListEnvironments returns the environments of a project.
func ListEnvironments(ctx context.Context, q db.Querier, project db.Project) ([]db.Environment, error) {
	environments, err := q.ListEnvironments(ctx, db.ListEnvironmentsParams{OrgID: project.OrgID, ProjectID: project.ID})
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	return environments, nil
}

// CreateProject creates a project with its default environments. The
// organization row lock serializes the choice of the organization's default
// project before the insert.
func CreateProject(ctx context.Context, txb db.TxBeginner, orgID uuid.UUID, input ProjectInput) (db.Project, []db.Environment, error) {
	if err := input.HistoryRetention.Validate(); err != nil {
		return db.Project{}, nil, err
	}
	if err := input.ExecutionLimits.Validate(); err != nil {
		return db.Project{}, nil, err
	}
	slug, name, err := normalizeProjectSlugName(input.Slug, input.Name)
	if err != nil {
		return db.Project{}, nil, err
	}
	if input.DefaultRegionID != "" {
		if err := region.ValidateID(input.DefaultRegionID); err != nil {
			return db.Project{}, nil, invalidInput("invalid default_region_id: %v", err)
		}
	}
	var project db.Project
	var environments []db.Environment
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockOrganizationForProjectDefaults(ctx, pgvalue.UUID(orgID)); err != nil {
			return fmt.Errorf("lock organization: %w", err)
		}
		regionID, err := defaultRegionID(ctx, q, input.DefaultRegionID)
		if err != nil {
			return err
		}
		created, err := q.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
			ID:                       pgvalue.UUID(uuid.NewV7()),
			OrgID:                    pgvalue.UUID(orgID),
			DefaultRegionID:          regionID,
			Slug:                     slug,
			Name:                     name,
			IsDefault:                false,
			EnvironmentID:            pgvalue.UUID(uuid.NewV7()),
			StagingEnvironmentID:     pgvalue.UUID(uuid.NewV7()),
			HistoryRetentionMode:     input.HistoryRetention.Mode,
			HistoryRetentionSeconds:  input.HistoryRetention.seconds(),
			MaxOutstandingAdmissions: input.ExecutionLimits.MaxOutstandingAdmissions,
			MaxCausalDepth:           int32(input.ExecutionLimits.MaxCausalDepth),
			MaxResidentComputers:     input.ExecutionLimits.MaxResidentComputers,
			MaxCpuMillis:             input.ExecutionLimits.MaxCPUMillis,
			MaxMemoryBytes:           input.ExecutionLimits.MaxMemoryBytes,
			MaxReservedStorageBytes:  input.ExecutionLimits.MaxReservedStorageBytes,
			PreparationTimeoutMs:     input.ExecutionLimits.PreparationTimeoutMS,
			AdmissionRatePerSecond:   input.ExecutionLimits.AdmissionRatePerSecond,
			AdmissionBurst:           input.ExecutionLimits.AdmissionBurst,
		})
		if db.IsUniqueViolation(err) {
			return ErrProjectSlugInUse
		}
		if err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		project = db.Project(created)
		environments, err = ListEnvironments(ctx, q, project)
		return err
	})
	if err != nil {
		return db.Project{}, nil, err
	}
	return project, environments, nil
}

func defaultRegionID(ctx context.Context, q db.Querier, requested string) (string, error) {
	if requested == "" {
		regions, err := q.ListRegions(ctx)
		if err != nil {
			return "", fmt.Errorf("list regions: %w", err)
		}
		if len(regions) == 0 {
			return "", ErrNoRegion
		}
		return regions[0].ID, nil
	}
	found, err := q.GetRegion(ctx, requested)
	if isNoRows(err) {
		return "", ErrDefaultRegionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load default region: %w", err)
	}
	return found.ID, nil
}

// UpdateProject changes the display name while preserving the public identifier.
func UpdateProject(ctx context.Context, q db.Querier, orgID uuid.UUID, projectID uuid.UUID, details ProjectDetails) (db.Project, error) {
	name, err := normalizeDisplayName(details.Name)
	if err != nil {
		return db.Project{}, err
	}
	project, err := q.UpdateProjectDetails(ctx, db.UpdateProjectDetailsParams{
		OrgID: pgvalue.UUID(orgID),
		ID:    pgvalue.UUID(projectID),
		Name:  name,
	})
	if isNoRows(err) {
		return db.Project{}, ErrProjectNotFound
	}
	if db.IsUniqueViolation(err) {
		return db.Project{}, ErrProjectSlugInUse
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("update project: %w", err)
	}
	return project, nil
}

// CreateEnvironment adds an environment to an active project.
func CreateEnvironment(ctx context.Context, txb db.TxBeginner, orgID uuid.UUID, projectID uuid.UUID, details EnvironmentDetails, limits ExecutionLimits) (db.Environment, error) {
	if err := details.HistoryRetention.Validate(); err != nil {
		return db.Environment{}, err
	}
	if err := limits.Validate(); err != nil {
		return db.Environment{}, err
	}
	slug, name, err := normalizeSlugName(details.Slug, details.Name)
	if err != nil {
		return db.Environment{}, err
	}
	var environment db.Environment
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		_, err := q.GetProject(ctx, db.GetProjectParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(projectID)})
		if isNoRows(err) {
			return ErrProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("load project: %w", err)
		}
		environment, err = q.CreateEnvironment(ctx, db.CreateEnvironmentParams{
			ID:                       pgvalue.UUID(uuid.NewV7()),
			OrgID:                    pgvalue.UUID(orgID),
			ProjectID:                pgvalue.UUID(projectID),
			Slug:                     slug,
			Name:                     name,
			ColorHex:                 details.ColorHex,
			IsDefault:                false,
			HistoryRetentionMode:     details.HistoryRetention.Mode,
			HistoryRetentionSeconds:  details.HistoryRetention.seconds(),
			MaxOutstandingAdmissions: limits.MaxOutstandingAdmissions,
			MaxCausalDepth:           int32(limits.MaxCausalDepth),
			MaxResidentComputers:     limits.MaxResidentComputers,
			MaxCpuMillis:             limits.MaxCPUMillis,
			MaxMemoryBytes:           limits.MaxMemoryBytes,
			MaxReservedStorageBytes:  limits.MaxReservedStorageBytes,
			PreparationTimeoutMs:     limits.PreparationTimeoutMS,
			AdmissionRatePerSecond:   limits.AdmissionRatePerSecond,
			AdmissionBurst:           limits.AdmissionBurst,
		})
		if db.IsUniqueViolation(err) {
			return ErrEnvironmentSlugInUse
		}
		if err != nil {
			return fmt.Errorf("create environment: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.Environment{}, err
	}
	return environment, nil
}

// UpdateEnvironment changes display settings while preserving the public identifier.
func UpdateEnvironment(ctx context.Context, database db.TxBeginner, actor audit.Actor, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, details EnvironmentDetails) (db.Environment, error) {
	if details.HistoryRetention.Mode != "" || details.HistoryRetention.Seconds != nil {
		if err := details.HistoryRetention.Validate(); err != nil {
			return db.Environment{}, err
		}
	}
	name, err := normalizeDisplayName(details.Name)
	if err != nil {
		return db.Environment{}, err
	}
	var environment db.Environment
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var oldMode string
		var oldSeconds pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT history_retention_mode,history_retention_seconds FROM environments WHERE org_id=$1 AND project_id=$2 AND id=$3 FOR NO KEY UPDATE`, orgID, projectID, environmentID).Scan(&oldMode, &oldSeconds); isNoRows(err) {
			return ErrEnvironmentNotFound
		} else if err != nil {
			return err
		}
		q := db.New(tx)
		environment, err = q.UpdateEnvironmentDetails(ctx, db.UpdateEnvironmentDetailsParams{
			OrgID:                   pgvalue.UUID(orgID),
			ProjectID:               pgvalue.UUID(projectID),
			ID:                      pgvalue.UUID(environmentID),
			Name:                    name,
			ColorHex:                details.ColorHex,
			HistoryRetentionMode:    details.HistoryRetention.Mode,
			HistoryRetentionSeconds: details.HistoryRetention.seconds(),
		})
		if isNoRows(err) {
			return ErrEnvironmentNotFound
		}
		if db.IsUniqueViolation(err) {
			return ErrEnvironmentSlugInUse
		}
		if err != nil {
			return fmt.Errorf("update environment: %w", err)
		}
		if oldMode != environment.HistoryRetentionMode || oldSeconds != environment.HistoryRetentionSeconds {
			var before, after *int64
			if oldSeconds.Valid {
				before = &oldSeconds.Int64
			}
			if environment.HistoryRetentionSeconds.Valid {
				after = &environment.HistoryRetentionSeconds.Int64
			}
			return audit.Append(ctx, tx, audit.Event{Actor: actor, OrgID: orgID, EnvironmentID: environmentID, Action: "environment.retention_changed", TargetKind: "environment", TargetID: environmentID, OldRetentionMode: oldMode, NewRetentionMode: environment.HistoryRetentionMode, OldRetentionSeconds: before, NewRetentionSeconds: after})
		}
		return nil
	})
	if err != nil {
		return db.Environment{}, err
	}

	return environment, nil
}

// GetEnvironmentByReference resolves a public Environment identifier in its Project.
func GetEnvironmentByReference(ctx context.Context, q db.Querier, orgID, projectID uuid.UUID, ref string) (db.Environment, error) {
	if !slugPattern.MatchString(ref) {
		return db.Environment{}, invalidInput("invalid environment identifier")
	}
	environment, err := q.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID), Slug: ref})
	if isNoRows(err) {
		return db.Environment{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return db.Environment{}, fmt.Errorf("load environment: %w", err)
	}
	return environment, nil
}
