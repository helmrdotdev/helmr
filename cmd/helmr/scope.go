package main

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

const (
	helmrProjectEnv     = "HELMR_PROJECT"
	helmrEnvironmentEnv = "HELMR_ENV"
)

// environmentScopeForClient resolves --project and --env, falling back to
// HELMR_PROJECT and HELMR_ENV for each value the flags leave empty. API keys
// are already Environment scoped, so a selected scope is sent as an
// expectation the server checks against the key.
func environmentScopeForClient(ctx context.Context, controlPlane *client.Client, projectID string, environmentID string) (client.EnvironmentScopeOptions, error) {
	scope := client.EnvironmentScopeOptions{
		ProjectID:     scopeValue(projectID, helmrProjectEnv),
		EnvironmentID: scopeValue(environmentID, helmrEnvironmentEnv),
	}
	if !controlPlane.UsesSessionScopedRoutes() {
		controlPlane.ExpectAPIKeyScope(scope)
		return client.EnvironmentScopeOptions{}, nil
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" {
		return client.EnvironmentScopeOptions{}, errors.New("--project and --env are required with helmr login; HELMR_PROJECT and HELMR_ENV can supply them")
	}
	if err := validateProjectFlag(scope.ProjectID); err != nil {
		return client.EnvironmentScopeOptions{}, err
	}
	project, environment, err := resolveProjectEnvironment(ctx, controlPlane, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return client.EnvironmentScopeOptions{}, err
	}
	return client.EnvironmentScopeOptions{ProjectID: project.Project, EnvironmentID: environment.Environment}, nil
}

func computerScopeForClient(ctx context.Context, controlPlane *client.Client, projectID string, environmentID string) (client.ComputerScopeOptions, error) {
	environmentScope, err := environmentScopeForClient(ctx, controlPlane, projectID, environmentID)
	return client.ComputerScopeOptions(environmentScope), err
}

func addScopeFlags(cmd *cobra.Command, projectID *string, environmentID *string) {
	cmd.Flags().StringVarP(projectID, "project", "p", "", "Project ID.")
	cmd.Flags().StringVarP(environmentID, "env", "e", "", "Environment ID.")
}

func scopeValue(flag, env string) string {
	if value := strings.TrimSpace(flag); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(env))
}

func validateProjectFlag(project string) error {
	if strings.Contains(project, "=") {
		return errors.New("--project must be a project ID")
	}
	return nil
}
