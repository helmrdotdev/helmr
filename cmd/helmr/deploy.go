package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/spf13/cobra"
)

var deployArchiveTempDir string

const deployDefaultWaitTimeout = 20 * time.Minute

var deployEventReconnectDelay = time.Second
var deployEventStatusInterval = 5 * time.Second

func deployCommand() *cobra.Command {
	var projectRef string
	var envRef string
	var detach bool
	var skipPromotion bool
	var timeout time.Duration
	var jsonOutput bool
	var idempotencyKey string
	var noImageCache bool
	cmd := &cobra.Command{
		Use:   "deploy [path]",
		Short: "Deploy tasks from a helmr.config.ts project.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceRoot := "."
			if len(args) > 0 {
				sourceRoot = args[0]
			}
			absRoot, err := filepath.Abs(sourceRoot)
			if err != nil {
				return err
			}
			info, err := os.Stat(absRoot)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("deploy path must be a directory: %s", sourceRoot)
			}
			reporter := newDeployReporter(cmd, jsonOutput)
			if err := reporter.Step("Creating archive"); err != nil {
				return err
			}
			tarArchive, cleanup, err := archive.CreateTarWithOptions(absRoot, deployArchiveTempDir, archive.TarOptions{
				CanonicalSource: true,
			})
			if err != nil {
				return err
			}
			defer cleanup()
			source, err := os.Open(tarArchive.Path)
			if err != nil {
				return err
			}
			_, inspectErr := deployment.InspectSource(source)
			closeErr := source.Close()
			if inspectErr != nil {
				return inspectErr
			}
			if closeErr != nil {
				return closeErr
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
			if err := reporter.Step("Uploading deployment"); err != nil {
				return err
			}
			deployRequest := api.CreateDeploymentRequest{
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
				ContentHash:    tarArchive.Digest,
				ImageCacheMode: "prefer",
			}
			if noImageCache {
				deployRequest.ImageCacheMode = "bypass"
			}
			if deployRequest.IdempotencyKey == "" {
				deployRequest.IdempotencyKey = uuid.Must(uuid.NewV7()).String()
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectRef, envRef)
			if err != nil {
				return err
			}
			response, err := controlPlane.CreateDeployment(cmd.Context(), deployRequest, tarArchive.Path, scope)
			if err != nil {
				return err
			}
			if err := reporter.DeploymentCreated(response); err != nil {
				return err
			}
			if detach {
				return reporter.DeploymentResult(response, "queued")
			}
			deployed, err := waitForDeployment(cmd.Context(), controlPlane, response, scope, timeout, reporter)
			if err != nil {
				return err
			}
			if skipPromotion {
				return reporter.DeploymentResult(deployed, "deployed")
			}
			if err := reporter.Step("Promoting deployment"); err != nil {
				return err
			}
			promoteRequest := api.PromoteDeploymentRequest{Reason: "deploy"}
			promoted, err := controlPlane.PromoteDeployment(cmd.Context(), deployed.ID, promoteRequest, scope)
			if err != nil {
				return err
			}
			return reporter.DeploymentResult(promoted, "promoted")
		},
	}
	cmd.Flags().StringVarP(&projectRef, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&envRef, "env", "e", "", "Environment slug or ID for this deployment.")
	cmd.Flags().BoolVar(&detach, "detach", false, "Queue the deployment build and return without promotion.")
	cmd.Flags().BoolVar(&skipPromotion, "skip-promotion", false, "Build the deployment without promoting it current.")
	cmd.Flags().DurationVar(&timeout, "timeout", deployDefaultWaitTimeout, "Maximum time to wait for deployment completion.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON lines for deployment progress.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for retrying deployment creation.")
	cmd.Flags().BoolVar(&noImageCache, "no-image-cache", false, "Build Workspace images without importing or exporting the Platform layer cache.")
	return cmd
}

type deploymentStatusClient interface {
	GetDeployment(context.Context, string, client.EnvironmentScopeOptions) (api.DeploymentResponse, error)
	FollowDeploymentEvents(context.Context, string, client.EnvironmentScopeOptions, string, func(api.RunEvent) error) error
}

type deployReporter interface {
	Step(string) error
	DeploymentCreated(api.DeploymentResponse) error
	Event(api.RunEvent) error
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
	Event      *api.RunEvent           `json:"event,omitempty"`
}

func newDeployReporter(cmd *cobra.Command, jsonOutput bool) deployReporter {
	return cliDeployReporter{cmd: cmd, jsonOutput: jsonOutput}
}

func (r cliDeployReporter) Step(message string) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "step", Step: message}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "%s\n", message)
	return err
}

func (r cliDeployReporter) DeploymentCreated(deployment api.DeploymentResponse) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_created", Deployment: &deployment}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "Deployment %s queued\n", deployment.ID)
	return err
}

func (r cliDeployReporter) Event(event api.RunEvent) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_event", Event: &event}})
	}
	message := strings.TrimSpace(event.Message)
	if message == "" {
		message = strings.TrimSpace(event.Kind)
	}
	if message != "" {
		if _, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "%s\n", message); err != nil {
			return err
		}
	}
	return nil
}

func (r cliDeployReporter) DeploymentResult(deployment api.DeploymentResponse, phase string) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_result", Phase: phase, Deployment: &deployment}})
	}
	_, err := fmt.Fprintln(r.cmd.OutOrStdout(), deploymentOutputRef(deployment))
	return err
}

func promoteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var reason string
	cmd := &cobra.Command{
		Use:   "promote DEPLOYMENT",
		Short: "Promote a deployed version to current.",
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
			deployment, err := controlPlane.PromoteDeployment(cmd.Context(), args[0], request, scope)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), deploymentOutputRef(deployment))
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project ID or slug for the deployment.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment ID or slug for the deployment.")
	cmd.Flags().StringVar(&reason, "reason", "", "Promotion reason.")
	return cmd
}

func deploymentOutputRef(deployment api.DeploymentResponse) string {
	if strings.TrimSpace(deployment.Version) != "" {
		return strings.TrimSpace(deployment.Version)
	}
	return deployment.ID
}

func waitForDeployment(ctx context.Context, controlPlane deploymentStatusClient, initial api.DeploymentResponse, scope client.EnvironmentScopeOptions, timeout time.Duration, reporter deployReporter) (api.DeploymentResponse, error) {
	if strings.TrimSpace(initial.ID) == "" {
		return api.DeploymentResponse{}, errors.New("deployment response id is empty")
	}
	if deploymentFinished(initial.Status) {
		return deploymentTerminalResult(initial)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var cursor string
	for {
		streamCtx, cancel := context.WithTimeout(ctx, deployEventStatusInterval)
		terminal := false
		err := controlPlane.FollowDeploymentEvents(streamCtx, initial.ID, scope, cursor, func(event api.RunEvent) error {
			if event.ID != "" {
				cursor = event.ID
			}
			if err := reporter.Event(event); err != nil {
				return err
			}
			switch event.Kind {
			case "deployment.deployed", "deployment.failed":
				terminal = true
				cancel()
			}
			return nil
		})
		streamContextErr := streamCtx.Err()
		cancel()
		if err != nil && (streamContextErr == nil || !errors.Is(err, streamContextErr)) {
			return api.DeploymentResponse{}, fmt.Errorf("follow deployment %s events: %w", initial.ID, err)
		}
		if ctx.Err() != nil {
			return api.DeploymentResponse{}, fmt.Errorf("wait for deployment %s: %w", initial.ID, ctx.Err())
		}
		deployment, err := controlPlane.GetDeployment(ctx, initial.ID, scope)
		if err != nil {
			return api.DeploymentResponse{}, fmt.Errorf("get deployment %s: %w", initial.ID, err)
		}
		if terminal || deploymentFinished(deployment.Status) {
			return deploymentTerminalResult(deployment)
		}
		timer := time.NewTimer(deployEventReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.DeploymentResponse{}, fmt.Errorf("wait for deployment %s: %w", initial.ID, ctx.Err())
		case <-timer.C:
		}
	}
}

func deploymentFinished(status api.DeploymentStatus) bool {
	switch status {
	case api.DeploymentStatusDeployed, api.DeploymentStatusFailed:
		return true
	default:
		return false
	}
}

func deploymentTerminalResult(deployment api.DeploymentResponse) (api.DeploymentResponse, error) {
	switch deployment.Status {
	case api.DeploymentStatusDeployed:
		return deployment, nil
	case api.DeploymentStatusFailed:
		message := strings.TrimSpace(deploymentErrorMessage(deployment))
		if message == "" {
			message = "deployment build failed"
		}
		return api.DeploymentResponse{}, fmt.Errorf("deployment %s failed: %s", deployment.ID, message)
	default:
		return api.DeploymentResponse{}, fmt.Errorf("deployment %s reached unexpected status %q", deployment.ID, deployment.Status)
	}
}

func deploymentErrorMessage(deployment api.DeploymentResponse) string {
	if deployment.Failure == nil {
		return ""
	}
	return strings.TrimSpace(deployment.Failure.Message)
}
