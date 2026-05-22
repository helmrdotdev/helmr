package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
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
		projectDeleteCommand(),
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
		envDeleteCommand(),
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
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			response, err := control.ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, project := range response.Projects {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", project.ID, project.Slug, project.Name)
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
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			project, err = control.GetProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), project)
			}
			return writeProject(cmd.OutOrStdout(), project)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func projectCreateCommand() *cobra.Command {
	var slug string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("project name is required")
			}
			slug = requestedSlug(slug, name)
			if slug == "" {
				return errors.New("project slug is required; pass --slug")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := control.CreateProject(cmd.Context(), api.CreateProjectRequest{Slug: slug, Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), project)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", project.ID, project.Slug, project.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "Project slug. Defaults to a slug generated from NAME.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func projectUpdateCommand() *cobra.Command {
	var name string
	var slug string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update PROJECT",
		Short: "Update a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("slug") {
				return errors.New("project update requires --name or --slug")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			project, err = control.GetProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") {
				name = project.Name
			}
			if !cmd.Flags().Changed("slug") {
				slug = project.Slug
			}
			name = strings.TrimSpace(name)
			slug = strings.TrimSpace(slug)
			if slug == "" {
				return errors.New("--slug cannot be empty")
			}
			updated, err := control.UpdateProject(cmd.Context(), project.ID, api.UpdateProjectRequest{Slug: slug, Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", updated.ID, updated.Slug, updated.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Project name.")
	cmd.Flags().StringVar(&slug, "slug", "", "Project slug.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func projectDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete PROJECT --yes",
		Short: "Delete a project.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("project delete requires --yes")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			if err := control.DeleteProject(cmd.Context(), project.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", project.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion.")
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
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), control, projectRef)
			if err != nil {
				return err
			}
			project, err = control.GetProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), project.Environments)
			}
			for _, environment := range project.Environments {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", environment.ID, environment.Slug, environment.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project slug or ID.")
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
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, environment, err := resolveProjectEnvironment(cmd.Context(), control, projectRef, args[0])
			if err != nil {
				return err
			}
			environment, err = control.GetEnvironment(cmd.Context(), project.ID, environment.ID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), environment)
			}
			return writeEnvironment(cmd.OutOrStdout(), environment)
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project slug or ID.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func envCreateCommand() *cobra.Command {
	var projectRef string
	var slug string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create NAME --project PROJECT",
		Short: "Create an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("environment name is required")
			}
			slug = requestedSlug(slug, name)
			if slug == "" {
				return errors.New("environment slug is required; pass --slug")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, err := resolveProject(cmd.Context(), control, projectRef)
			if err != nil {
				return err
			}
			environment, err := control.CreateEnvironment(cmd.Context(), project.ID, api.CreateEnvironmentRequest{Slug: slug, Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), environment)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", environment.ID, environment.Slug, environment.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project slug or ID.")
	cmd.Flags().StringVar(&slug, "slug", "", "Environment slug. Defaults to a slug generated from NAME.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func envUpdateCommand() *cobra.Command {
	var projectRef string
	var name string
	var slug string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update ENVIRONMENT --project PROJECT",
		Short: "Update an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") && !cmd.Flags().Changed("slug") {
				return errors.New("environment update requires --name or --slug")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, environment, err := resolveProjectEnvironment(cmd.Context(), control, projectRef, args[0])
			if err != nil {
				return err
			}
			environment, err = control.GetEnvironment(cmd.Context(), project.ID, environment.ID)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") {
				name = environment.Name
			}
			if !cmd.Flags().Changed("slug") {
				slug = environment.Slug
			}
			name = strings.TrimSpace(name)
			slug = strings.TrimSpace(slug)
			if slug == "" {
				return errors.New("--slug cannot be empty")
			}
			updated, err := control.UpdateEnvironment(cmd.Context(), project.ID, environment.ID, api.UpdateEnvironmentRequest{Slug: slug, Name: name})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", updated.ID, updated.Slug, updated.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project slug or ID.")
	cmd.Flags().StringVar(&name, "name", "", "Environment name.")
	cmd.Flags().StringVar(&slug, "slug", "", "Environment slug.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func envDeleteCommand() *cobra.Command {
	var projectRef string
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete ENVIRONMENT --project PROJECT --yes",
		Short: "Delete an environment.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRef, err := requireProjectFlag(cmd)
			if err != nil {
				return err
			}
			if !yes {
				return errors.New("environment delete requires --yes")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			project, environment, err := resolveProjectEnvironment(cmd.Context(), control, projectRef, args[0])
			if err != nil {
				return err
			}
			if err := control.DeleteEnvironment(cmd.Context(), project.ID, environment.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", environment.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectRef, "project", "", "Project slug or ID.")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion.")
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

func resolveProject(ctx context.Context, control *client.Client, ref string) (api.ProjectSummary, error) {
	response, err := control.ListProjects(ctx)
	if err != nil {
		return api.ProjectSummary{}, err
	}
	ref = strings.TrimSpace(ref)
	for _, project := range response.Projects {
		if project.ID == ref || project.Slug == strings.ToLower(ref) {
			return project, nil
		}
	}
	return api.ProjectSummary{}, fmt.Errorf("project %q not found", ref)
}

func resolveProjectEnvironment(ctx context.Context, control *client.Client, projectRef string, environmentRef string) (api.ProjectSummary, api.EnvironmentSummary, error) {
	project, err := resolveProject(ctx, control, projectRef)
	if err != nil {
		return api.ProjectSummary{}, api.EnvironmentSummary{}, err
	}
	environmentRef = strings.TrimSpace(environmentRef)
	for _, environment := range project.Environments {
		if environment.ID == environmentRef || environment.Slug == strings.ToLower(environmentRef) {
			return project, environment, nil
		}
	}
	return api.ProjectSummary{}, api.EnvironmentSummary{}, fmt.Errorf("environment %q not found in project %q", environmentRef, projectRef)
}

func requestedSlug(raw string, name string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		return strings.ToLower(raw)
	}
	return slugify(name)
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastHyphen := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastHyphen = false
		case unicode.IsSpace(r) || r == '-':
			if builder.Len() > 0 && !lastHyphen {
				builder.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return strings.Trim(slug, "-")
}

func writeProject(w io.Writer, project api.ProjectSummary) error {
	fmt.Fprintf(w, "ID: %s\n", project.ID)
	fmt.Fprintf(w, "Slug: %s\n", project.Slug)
	fmt.Fprintf(w, "Name: %s\n", project.Name)
	if len(project.Environments) == 0 {
		return nil
	}
	fmt.Fprintln(w, "Environments:")
	for _, environment := range project.Environments {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", environment.ID, environment.Slug, environment.Name)
	}
	return nil
}

func writeEnvironment(w io.Writer, environment api.EnvironmentSummary) error {
	fmt.Fprintf(w, "ID: %s\n", environment.ID)
	fmt.Fprintf(w, "Project: %s\n", environment.ProjectID)
	fmt.Fprintf(w, "Slug: %s\n", environment.Slug)
	fmt.Fprintf(w, "Name: %s\n", environment.Name)
	return nil
}
