package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func projectCommand() *cobra.Command {
	project := &cobra.Command{
		Use:   "project",
		Short: "Manage projects.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	project.AddCommand(
		projectListCommand(),
		projectGetCommand(),
		projectCreateCommand(),
		projectUpdateCommand(),
	)
	return project
}

func envCommand() *cobra.Command {
	env := &cobra.Command{
		Use:   "env",
		Short: "Manage project environments.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	env.AddCommand(
		envListCommand(),
		envGetCommand(),
		envCreateCommand(),
		envUpdateCommand(),
	)
	return env
}

func projectListCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List projects.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			response := api.ListProjectsResponse{Projects: []api.ProjectSummary{}}
			cursor := ""
			for {
				page, err := controlPlane.ListProjects(cmd.Context(), client.ListProjectsOptions{
					Cursor: cursor, Limit: 100,
				})
				if err != nil {
					return err
				}
				response.Projects = append(response.Projects, page.Projects...)
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			for _, project := range response.Projects {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", project.Project, project.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func projectGetCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get PROJECT",
		Short: "Show a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), controlPlane, args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), project)
			}
			return writeProject(cmd.OutOrStdout(), project)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func projectCreateCommand() *cobra.Command {
	var name string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create PROJECT",
		Short: "Create a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			retention, err := historyRetentionFlags(cmd, true)
			if err != nil {
				return err
			}
			slug := strings.TrimSpace(args[0])
			if name == "" {
				name = slug
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, err := controlPlane.CreateProject(cmd.Context(), api.CreateProjectRequest{HistoryRetentionPolicy: retention, Project: slug, Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), project)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", project.Project, project.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name. Defaults to the identifier.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	addHistoryRetentionFlags(cmd)
	return cmd
}

func projectUpdateCommand() *cobra.Command {
	var name string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update PROJECT",
		Short: "Update a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("name") {
				return errors.New("project update requires --name")
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), controlPlane, args[0])
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") {
				name = project.Name
			}
			name = strings.TrimSpace(name)
			updated, err := controlPlane.UpdateProject(cmd.Context(), project.Project, api.UpdateProjectRequest{Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", updated.Project, updated.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Project name.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func envListCommand() *cobra.Command {
	var projectRef string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list --project PROJECT",
		Short: "List environments.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), controlPlane, projectRef)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), project.Environments)
			}
			for _, environment := range project.Environments {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", environment.Environment, environment.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON array.")
	return cmd
}

func envGetCommand() *cobra.Command {
	var projectRef string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get ENVIRONMENT --project PROJECT",
		Short: "Show an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, environment, err := resolveProjectEnvironment(cmd.Context(), controlPlane, projectRef, args[0])
			if err != nil {
				return err
			}
			environment, err = controlPlane.GetEnvironment(cmd.Context(), project.Project, environment.Environment)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), environment)
			}
			return writeEnvironment(cmd.OutOrStdout(), environment)
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func envCreateCommand() *cobra.Command {
	var projectRef string
	var name string
	var colorHex string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create ENVIRONMENT --project PROJECT",
		Short: "Create an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			retention, err := historyRetentionFlags(cmd, true)
			if err != nil {
				return err
			}
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			slug := strings.TrimSpace(args[0])
			if name == "" {
				name = slug
			}
			if !cmd.Flags().Changed("color") {
				colorHex = defaultEnvironmentColorHex(slug)
			}
			colorHex, err = normalizeEnvironmentColorFlag(colorHex)
			if err != nil {
				return err
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), controlPlane, projectRef)
			if err != nil {
				return err
			}
			environment, err := controlPlane.CreateEnvironment(cmd.Context(), project.Project, api.CreateEnvironmentRequest{HistoryRetentionPolicy: retention, Environment: slug, Name: name, ColorHex: colorHex})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), environment)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", environment.Environment, environment.Name, environment.ColorHex)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project ID.")
	cmd.Flags().StringVar(&name, "name", "", "Display name. Defaults to the identifier.")
	cmd.Flags().StringVar(&colorHex, "color", "", "Environment color as #RRGGBB. Defaults from the Environment ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	addHistoryRetentionFlags(cmd)
	return cmd
}

func envUpdateCommand() *cobra.Command {
	var projectRef string
	var name string
	var colorHex string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update ENVIRONMENT --project PROJECT",
		Short: "Update an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			retention, err := historyRetentionFlags(cmd, false)
			if err != nil {
				return err
			}
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("color") && !cmd.Flags().Changed("history-retention-mode") && !cmd.Flags().Changed("history-retention-seconds") {
				return errors.New("environment update requires a name, color, or history retention change")
			}
			controlPlane, err := sessionControlPlaneClient(cmd)
			if err != nil {
				return err
			}
			project, environment, err := resolveProjectEnvironment(cmd.Context(), controlPlane, projectRef, args[0])
			if err != nil {
				return err
			}
			environment, err = controlPlane.GetEnvironment(cmd.Context(), project.Project, environment.Environment)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") {
				name = environment.Name
			}
			if !cmd.Flags().Changed("color") {
				colorHex = environment.ColorHex
			}
			name = strings.TrimSpace(name)
			colorHex, err = normalizeEnvironmentColorFlag(colorHex)
			if err != nil {
				return err
			}
			updated, err := controlPlane.UpdateEnvironment(cmd.Context(), project.Project, environment.Environment, api.UpdateEnvironmentRequest{HistoryRetentionPolicy: retention, Name: name, ColorHex: colorHex})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", updated.Environment, updated.Name, updated.ColorHex)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project ID.")
	cmd.Flags().StringVar(&name, "name", "", "Environment name.")
	cmd.Flags().StringVar(&colorHex, "color", "", "Environment color as #RRGGBB.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	addHistoryRetentionFlags(cmd)
	return cmd
}

func requireProjectFlag(cmd *cobra.Command) (string, error) {
	projectRef, err := cmd.Flags().GetString("project")
	if err != nil {
		return "", err
	}
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		return "", errors.New("--project is required")
	}
	return projectRef, nil
}

func resolveProject(ctx context.Context, controlPlane *client.Client, ref string) (api.ProjectSummary, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return api.ProjectSummary{}, errors.New("project reference is required")
	}
	return controlPlane.GetProject(ctx, ref)
}

func resolveProjectEnvironment(ctx context.Context, controlPlane *client.Client, projectRef string, environmentRef string) (api.ProjectSummary, api.EnvironmentSummary, error) {
	project, err := resolveProject(ctx, controlPlane, projectRef)
	if err != nil {
		return api.ProjectSummary{}, api.EnvironmentSummary{}, err
	}
	environmentRef = strings.TrimSpace(environmentRef)
	for _, environment := range project.Environments {
		if environment.Environment == environmentRef {
			return project, environment, nil
		}
	}
	return api.ProjectSummary{}, api.EnvironmentSummary{}, fmt.Errorf("environment %q not found in project %q", environmentRef, projectRef)
}

func writeProject(w io.Writer, project api.ProjectSummary) error {
	fmt.Fprintf(w, "ID: %s\n", project.Project)
	fmt.Fprintf(w, "Name: %s\n", project.Name)
	if len(project.Environments) == 0 {
		return nil
	}
	fmt.Fprintln(w, "Environments:")
	for _, environment := range project.Environments {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", environment.Environment, environment.Name, environment.ColorHex)
	}
	return nil
}

func writeEnvironment(w io.Writer, environment api.EnvironmentSummary) error {
	fmt.Fprintf(w, "ID: %s\n", environment.Environment)
	fmt.Fprintf(w, "Project: %s\n", environment.Project)
	fmt.Fprintf(w, "Name: %s\n", environment.Name)
	fmt.Fprintf(w, "Color: %s\n", environment.ColorHex)
	fmt.Fprintf(w, "History retention: %s\n", environment.HistoryRetentionMode)
	if environment.HistoryRetentionSeconds != nil {
		fmt.Fprintf(w, "History retention seconds: %d\n", *environment.HistoryRetentionSeconds)
	}
	return nil
}

func normalizeEnvironmentColorFlag(colorHex string) (string, error) {
	normalized, err := api.NormalizeEnvironmentColorHex(colorHex)
	if err != nil {
		return "", errors.New("environment color must be a #RRGGBB color")
	}
	return normalized, nil
}

func defaultEnvironmentColorHex(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	switch {
	case strings.Contains(slug, "prod") || strings.Contains(slug, "live") || strings.Contains(slug, "main") || strings.Contains(slug, "master"):
		return "#315FCE"
	case strings.Contains(slug, "stag") || strings.Contains(slug, "qa"):
		return "#F59E0B"
	case strings.Contains(slug, "preview"):
		return "#06B6D4"
	case strings.Contains(slug, "dev") || strings.Contains(slug, "local"):
		return "#22C55E"
	case strings.Contains(slug, "sandbox") || strings.Contains(slug, "test"):
		return "#8B5CF6"
	case strings.Contains(slug, "demo") || strings.Contains(slug, "playground"):
		return "#8B5CF6"
	default:
		return customEnvironmentColorHex(slug)
	}
}

func customEnvironmentColorHex(slug string) string {
	palette := []string{"#0EA5E9", "#8B5CF6", "#EC4899", "#F97316", "#14B8A6", "#84CC16", "#6366F1"}
	if slug == "" {
		return palette[0]
	}
	var hash uint32
	for _, r := range slug {
		hash = hash*31 + uint32(r)
	}
	return palette[int(hash%uint32(len(palette)))]
}

func addHistoryRetentionFlags(cmd *cobra.Command) {
	cmd.Flags().String("history-retention-mode", "", "Session history policy: duration or until_environment_deletion. Required when creating.")
	cmd.Flags().Int64("history-retention-seconds", 0, "Positive seconds after Session termination and obligation release; required only for duration mode.")
}
func historyRetentionFlags(cmd *cobra.Command, required bool) (api.HistoryRetentionPolicy, error) {
	mode, _ := cmd.Flags().GetString("history-retention-mode")
	seconds, _ := cmd.Flags().GetInt64("history-retention-seconds")
	hasSeconds := cmd.Flags().Changed("history-retention-seconds")
	p := api.HistoryRetentionPolicy{HistoryRetentionMode: mode}
	if !required && !cmd.Flags().Changed("history-retention-mode") && !hasSeconds {
		return p, nil
	}
	if mode == "until_environment_deletion" && !hasSeconds {
		return p, nil
	}
	if mode == "duration" && hasSeconds && seconds > 0 {
		p.HistoryRetentionSeconds = &seconds
		return p, nil
	}
	return p, errors.New("choose --history-retention-mode duration with positive --history-retention-seconds, or --history-retention-mode until_environment_deletion without seconds")
}
