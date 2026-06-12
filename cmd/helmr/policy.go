package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func policyCommand() *cobra.Command {
	policy := &cobra.Command{Use: "policy", Short: "Manage waitpoint policies."}
	policy.AddCommand(
		policyListCommand(),
		policyGetCommand(),
		policyApplyCommand(),
		policyDeleteCommand(),
	)
	return policy
}

func policyListCommand() *cobra.Command {
	var jsonOutput bool
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List waitpoint policies.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := policyCommandScope(cmd.Context(), control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListWaitpointPolicies(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, policy := range response.Policies {
				fmt.Fprintln(cmd.OutOrStdout(), policy.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	return cmd
}

func policyGetCommand() *cobra.Command {
	var jsonOutput bool
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Show a waitpoint policy.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := policyCommandScope(cmd.Context(), control, projectID, environmentID)
			if err != nil {
				return err
			}
			policy, err := control.GetWaitpointPolicy(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), policy)
			}
			return writeWaitpointPolicy(cmd.OutOrStdout(), policy)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	return cmd
}

func policyApplyCommand() *cobra.Command {
	var file string
	var readStdin bool
	var label string
	var emails []string
	var jsonOutput bool
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "apply NAME",
		Short: "Create or update a waitpoint policy.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := waitpointPolicyApplyRequest(cmd.InOrStdin(), policyApplyOptions{
				File:   file,
				Stdin:  readStdin,
				Label:  label,
				Emails: emails,
			})
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := policyCommandScope(cmd.Context(), control, projectID, environmentID)
			if err != nil {
				return err
			}
			policy, err := control.ApplyWaitpointPolicy(cmd.Context(), args[0], request, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), policy)
			}
			fmt.Fprintln(cmd.OutOrStdout(), policy.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Read policy JSON from a file.")
	cmd.Flags().BoolVar(&readStdin, "stdin", false, "Read policy JSON from stdin.")
	cmd.Flags().StringVar(&label, "label", "", "Policy label for --email.")
	cmd.Flags().StringArrayVar(&emails, "email", nil, "Add an email recipient.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	return cmd
}

func policyDeleteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a waitpoint policy.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := policyCommandScope(cmd.Context(), control, projectID, environmentID)
			if err != nil {
				return err
			}
			if err := control.DeleteWaitpointPolicy(cmd.Context(), args[0], scope); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), args[0])
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	return cmd
}

func policyCommandScope(ctx context.Context, control *client.Client, projectID string, environmentID string) (client.RunScopeOptions, error) {
	return waitpointCommandScope(ctx, control, projectID, environmentID)
}

func writeWaitpointPolicy(w io.Writer, policy api.WaitpointPolicyResponse) error {
	fmt.Fprintf(w, "Name: %s\n", policy.Name)
	if strings.TrimSpace(policy.Label) != "" {
		fmt.Fprintf(w, "Label: %s\n", policy.Label)
	}
	if len(policy.Config) == 0 {
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, policy.Config, "", "  "); err != nil {
		fmt.Fprintf(w, "Config: %s\n", strings.TrimSpace(string(policy.Config)))
		return nil
	}
	fmt.Fprintln(w, "Config:")
	_, err := pretty.WriteTo(w)
	if err != nil {
		return err
	}
	fmt.Fprintln(w)
	return nil
}

type policyApplyOptions struct {
	File   string
	Stdin  bool
	Label  string
	Emails []string
}

type policyDocument struct {
	Label      string                        `json:"label,omitempty"`
	Reviewers  []api.WaitpointPolicyRule     `json:"reviewers,omitempty"`
	Deliveries []api.WaitpointPolicyDelivery `json:"deliveries,omitempty"`
	OnTimeout  *api.WaitpointPolicyTimeout   `json:"on_timeout,omitempty"`
	Config     json.RawMessage               `json:"config,omitempty"`
}

func waitpointPolicyApplyRequest(stdin io.Reader, opts policyApplyOptions) (api.UpdateWaitpointPolicyRequest, error) {
	file := strings.TrimSpace(opts.File)
	label := strings.TrimSpace(opts.Label)
	hasEmails := len(opts.Emails) > 0
	sourceCount := 0
	if file != "" {
		sourceCount++
	}
	if opts.Stdin {
		sourceCount++
	}
	if hasEmails {
		sourceCount++
	}
	if sourceCount == 0 {
		return api.UpdateWaitpointPolicyRequest{}, errors.New("policy apply requires --file, --stdin, or at least one --email")
	}
	if sourceCount > 1 {
		return api.UpdateWaitpointPolicyRequest{}, errors.New("--file, --stdin, and --email cannot be combined")
	}
	if file != "" {
		bytes, err := os.ReadFile(file)
		if err != nil {
			return api.UpdateWaitpointPolicyRequest{}, fmt.Errorf("read --file: %w", err)
		}
		return waitpointPolicyRequestFromJSON(bytes)
	}
	if opts.Stdin {
		bytes, err := io.ReadAll(stdin)
		if err != nil {
			return api.UpdateWaitpointPolicyRequest{}, err
		}
		return waitpointPolicyRequestFromJSON(bytes)
	}
	recipients := make([]string, 0, len(opts.Emails))
	for _, email := range opts.Emails {
		email = strings.TrimSpace(email)
		if email != "" {
			recipients = append(recipients, email)
		}
	}
	if len(recipients) == 0 {
		return api.UpdateWaitpointPolicyRequest{}, errors.New("--email requires at least one non-empty recipient")
	}
	config := api.WaitpointPolicyConfig{
		Deliveries: []api.WaitpointPolicyDelivery{{Type: "email", To: recipients}},
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return api.UpdateWaitpointPolicyRequest{}, err
	}
	return api.UpdateWaitpointPolicyRequest{Label: label, Config: configJSON}, nil
}

func waitpointPolicyRequestFromJSON(bytes []byte) (api.UpdateWaitpointPolicyRequest, error) {
	if !json.Valid(bytes) {
		return api.UpdateWaitpointPolicyRequest{}, errors.New("policy JSON must be valid JSON")
	}
	var document policyDocument
	if err := json.Unmarshal(bytes, &document); err != nil {
		return api.UpdateWaitpointPolicyRequest{}, err
	}
	config := document.Config
	if len(config) == 0 {
		configPayload := api.WaitpointPolicyConfig{
			Reviewers:  document.Reviewers,
			Deliveries: document.Deliveries,
			OnTimeout:  document.OnTimeout,
		}
		var err error
		config, err = json.Marshal(configPayload)
		if err != nil {
			return api.UpdateWaitpointPolicyRequest{}, err
		}
	}
	return api.UpdateWaitpointPolicyRequest{
		Label:  strings.TrimSpace(document.Label),
		Config: config,
	}, nil
}
