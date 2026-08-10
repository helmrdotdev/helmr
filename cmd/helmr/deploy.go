package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/spf13/cobra"
)

func deployCommand() *cobra.Command {
	var projectRef string
	var envRef string
	var bundlePath string
	var installCommand string
	var skipPromotion bool
	var jsonOutput bool
	var idempotencyKey string
	cmd := &cobra.Command{
		Use:   "deploy [path]",
		Short: "Build and deploy a verified deployment bundle.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (returnErr error) {
			if strings.TrimSpace(bundlePath) != "" && len(args) != 0 {
				return errors.New("deploy accepts either a source path or --bundle, not both")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			if !controlPlane.UsesSessionScopedRoutes() && (cmd.Flags().Changed("project") || cmd.Flags().Changed("env")) {
				return errors.New("--project and --env require helmr login; API keys are already environment scoped")
			}
			if controlPlane.UsesSessionScopedRoutes() {
				projectRef = strings.TrimSpace(projectRef)
				envRef = strings.TrimSpace(envRef)
				if !cmd.Flags().Changed("project") || projectRef == "" {
					return errors.New("--project is required with helmr login")
				}
				if !cmd.Flags().Changed("env") || envRef == "" {
					return errors.New("--env is required with helmr login")
				}
				if err := validateProjectFlag(projectRef); err != nil {
					return err
				}
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectRef, envRef)
			if err != nil {
				return err
			}
			reporter := newDeployReporter(cmd, jsonOutput)
			if strings.TrimSpace(bundlePath) == "" {
				source := "."
				if len(args) == 1 {
					source = args[0]
				}
				stage, err := os.MkdirTemp("", "helmr-deploy-")
				if err != nil {
					return err
				}
				defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
				bundlePath = filepath.Join(stage, "bundle")
				if err := reporter.Step("Building deployment bundle"); err != nil {
					return err
				}
				if err := buildDeploymentBundleAt(
					cmd.Context(), cmd, source, bundlePath, installCommand, false,
				); err != nil {
					return err
				}
			} else if strings.TrimSpace(installCommand) != "" {
				return errors.New("--install-command cannot be used with --bundle")
			}
			bundle, err := deployment.ReadDeploymentBundleDirectory(bundlePath)
			if err != nil {
				return fmt.Errorf("read deployment bundle: %w", err)
			}
			if err := reporter.Step("Planning deployment upload"); err != nil {
				return err
			}
			plan, err := controlPlane.PlanDeploymentBundleUploads(cmd.Context(), bundle.BundleJSON, scope)
			if err != nil {
				return err
			}
			if plan.BundleDigest != bundle.Digest {
				return errors.New("deployment upload plan returned a different bundle digest")
			}
			seen := make(map[string]struct{}, len(plan.Uploads))
			for _, upload := range plan.Uploads {
				if _, duplicate := seen[upload.Digest]; duplicate {
					return errors.New("deployment upload plan contains a duplicate object")
				}
				seen[upload.Digest] = struct{}{}
				path, ok := bundle.Objects[upload.Digest]
				if !ok {
					return errors.New("deployment upload plan requested an object outside the bundle")
				}
				if err := controlPlane.UploadDeploymentBundleObject(cmd.Context(), upload, path); err != nil {
					return fmt.Errorf("upload deployment object %s: %w", upload.Digest, err)
				}
			}
			idempotencyKey = strings.TrimSpace(idempotencyKey)
			if idempotencyKey == "" {
				idempotencyKey = uuid.Must(uuid.NewV7()).String()
			}
			if err := reporter.Step("Finalizing deployment"); err != nil {
				return err
			}
			created, err := controlPlane.FinalizeDeploymentBundle(cmd.Context(), api.FinalizeDeploymentBundleRequest{
				IdempotencyKey: idempotencyKey,
				BundleDigest:   bundle.Digest,
			}, scope)
			if err != nil {
				return err
			}
			if err := reporter.DeploymentCreated(created); err != nil {
				return err
			}
			if skipPromotion {
				return reporter.DeploymentResult(created, "finalized")
			}
			if err := reporter.Step("Promoting deployment"); err != nil {
				return err
			}
			promoted, err := controlPlane.PromoteDeployment(
				cmd.Context(), created.ID, api.PromoteDeploymentRequest{Reason: "deploy"}, scope,
			)
			if err != nil {
				return err
			}
			return reporter.DeploymentResult(promoted, "promoted")
		},
	}
	cmd.Flags().StringVarP(&projectRef, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&envRef, "env", "e", "", "Environment slug or ID for this deployment.")
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "Existing verified deployment bundle directory.")
	cmd.Flags().StringVar(&installCommand, "install-command", "", "Custom dependency installation/preparation command inside BuildKit.")
	cmd.Flags().BoolVar(&skipPromotion, "skip-promotion", false, "Finalize the deployment without promoting it current.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON lines for deployment progress.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for retrying deployment finalization.")
	return cmd
}

type deployReporter interface {
	Step(string) error
	DeploymentCreated(api.DeploymentResponse) error
	DeploymentResult(api.DeploymentResponse, string) error
}

type cliDeployReporter struct {
	cmd        *cobra.Command
	jsonOutput bool
}

type cliDeployLine struct {
	Type       string                  `json:"type"`
	Step       string                  `json:"step,omitempty"`
	Phase      string                  `json:"phase,omitempty"`
	Deployment *api.DeploymentResponse `json:"deployment,omitempty"`
}

func newDeployReporter(cmd *cobra.Command, jsonOutput bool) deployReporter {
	return cliDeployReporter{cmd: cmd, jsonOutput: jsonOutput}
}

func (r cliDeployReporter) Step(message string) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "step", Step: message}})
	}
	_, err := fmt.Fprintln(r.cmd.ErrOrStderr(), message)
	return err
}

func (r cliDeployReporter) DeploymentCreated(value api.DeploymentResponse) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_finalized", Deployment: &value}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "Deployment %s finalized\n", value.ID)
	return err
}

func (r cliDeployReporter) DeploymentResult(value api.DeploymentResponse, phase string) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_result", Phase: phase, Deployment: &value}})
	}
	_, err := fmt.Fprintln(r.cmd.OutOrStdout(), deploymentOutputRef(value))
	return err
}

func promoteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var reason string
	cmd := &cobra.Command{
		Use:   "promote DEPLOYMENT",
		Short: "Promote an immutable deployment to current.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			if !controlPlane.UsesSessionScopedRoutes() && (cmd.Flags().Changed("project") || cmd.Flags().Changed("env")) {
				return errors.New("--project and --env require helmr login; API keys are already environment scoped")
			}
			request := api.PromoteDeploymentRequest{Reason: strings.TrimSpace(reason)}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			value, err := controlPlane.PromoteDeployment(cmd.Context(), args[0], request, scope)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), deploymentOutputRef(value))
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project ID or slug for the deployment.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment ID or slug for the deployment.")
	cmd.Flags().StringVar(&reason, "reason", "", "Promotion reason.")
	return cmd
}

func deploymentOutputRef(value api.DeploymentResponse) string {
	if strings.TrimSpace(value.Version) != "" {
		return strings.TrimSpace(value.Version)
	}
	return value.ID
}
