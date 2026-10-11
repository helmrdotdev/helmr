package main

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
	"strings"
)

func environmentScopeForClient(ctx context.Context, controlPlane *client.Client, projectID string, environmentID string) (client.EnvironmentScopeOptions, error) {
	scope := client.EnvironmentScopeOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
	if !controlPlane.UsesSessionScopedRoutes() {
		if scope.ProjectID != "" || scope.EnvironmentID != "" {
			return client.EnvironmentScopeOptions{}, errors.New("--project and --env require helmr login; API keys are already environment scoped")
		}
		return client.EnvironmentScopeOptions{}, nil
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" {
		return client.EnvironmentScopeOptions{}, errors.New("--project and --env are required with helmr login")
	}
	project, environment, err := resolveProjectEnvironment(ctx, controlPlane, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return client.EnvironmentScopeOptions{}, err
	}
	return client.EnvironmentScopeOptions{ProjectID: project.ID, EnvironmentID: environment.ID}, nil
}

func computerScopeForClient(ctx context.Context, controlPlane *client.Client, projectID string, environmentID string) (client.ComputerScopeOptions, error) {
	environmentScope, err := environmentScopeForClient(ctx, controlPlane, projectID, environmentID)
	return client.ComputerScopeOptions(environmentScope), err
}

func addScopeFlags(cmd *cobra.Command, projectID *string, environmentID *string) {
	cmd.Flags().StringVarP(projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(environmentID, "env", "e", "", "Environment slug or ID.")
}

func validateProjectFlag(project string) error {
	if strings.Contains(project, "=") {
		return errors.New("--project must be a project slug or ID")
	}
	return nil
}
