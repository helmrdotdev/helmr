package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func deployCommand() *cobra.Command {
	var projectRef string
	var envRef string
	var bundlePath string
	var refreshDependencies bool
	var skipPromotion bool
	var jsonOutput bool
	var idempotencyKey string
	uploadOptions := client.DefaultDeploymentUploadOptions()
	cmd := &cobra.Command{
		Use:   "deploy [path]",
		Short: "Build and deploy a verified deployment bundle.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (returnErr error) {
			if strings.TrimSpace(bundlePath) != "" && refreshDependencies {
				return errors.New("--refresh-dependencies requires a source build, not --bundle")
			}
			if strings.TrimSpace(bundlePath) != "" && len(args) != 0 {
				return errors.New("deploy accepts either a source path or --bundle, not both")
			}
			for _, option := range []struct {
				flag, environment string
				target            *time.Duration
			}{
				{"upload-timeout", "HELMR_UPLOAD_TIMEOUT", &uploadOptions.Timeout},
				{"upload-idle-timeout", "HELMR_UPLOAD_IDLE_TIMEOUT", &uploadOptions.IdleTimeout},
			} {
				if value, ok := os.LookupEnv(option.environment); ok && !cmd.Flags().Changed(option.flag) {
					duration, err := time.ParseDuration(strings.TrimSpace(value))
					if err != nil {
						return fmt.Errorf("%s: %w", option.environment, err)
					}
					*option.target = duration
				}
			}
			if err := uploadOptions.Validate(); err != nil {
				return err
			}
			controlPlane, err := controlPlaneClient(cmd, client.WithDeploymentUploads(uploadOptions))
			if err != nil {
				return err
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
				if err := buildDeploymentBundleAt(cmd.Context(), cmd, source, bundlePath, false, refreshDependencies); err != nil {
					return err
				}
			}
			directory, err := bundle.ReadDirectory(bundlePath)
			if err != nil {
				return fmt.Errorf("read deployment bundle: %w", err)
			}
			if err := reporter.Step("Planning deployment upload"); err != nil {
				return err
			}
			if err := uploadDeploymentBundleObjects(
				cmd.Context(), controlPlane, directory, scope, !skipPromotion, reporter,
			); err != nil {
				return err
			}
			idempotencyKey = strings.TrimSpace(idempotencyKey)
			if idempotencyKey == "" {
				idempotencyKey = uuid.NewV7().String()
			}
			if err := reporter.Step("Finalizing deployment"); err != nil {
				return err
			}
			created, err := controlPlane.FinalizeDeploymentBundle(cmd.Context(), api.FinalizeDeploymentBundleRequest{
				IdempotencyKey: idempotencyKey,
				BundleDigest:   directory.Digest,
			}, scope, reporter.DeploymentObjectVerified)
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
				cmd.Context(), created.ID, scope,
			)
			if err != nil {
				return err
			}
			return reporter.DeploymentResult(promoted, "promoted")
		},
	}
	cmd.Flags().StringVarP(&projectRef, "project", "p", "", "Project ID.")
	cmd.Flags().StringVarP(&envRef, "env", "e", "", "Environment ID for this deployment.")
	cmd.Flags().BoolVar(&refreshDependencies, "refresh-dependencies", false, "Reinstall source and runtime dependencies without cached install layers.")
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "Existing verified deployment bundle directory.")
	cmd.Flags().BoolVar(&skipPromotion, "skip-promotion", false, "Finalize the deployment without promoting it current.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON lines for deployment progress.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for retrying deployment finalization.")
	cmd.Flags().DurationVar(&uploadOptions.Timeout, "upload-timeout", uploadOptions.Timeout, "Deadline for each object upload including capacity retries; HELMR_UPLOAD_TIMEOUT overrides the default.")
	cmd.Flags().DurationVar(&uploadOptions.IdleTimeout, "upload-idle-timeout", uploadOptions.IdleTimeout, "Maximum time without upload progress; HELMR_UPLOAD_IDLE_TIMEOUT overrides the default.")
	return cmd
}

func uploadDeploymentBundleObjects(
	ctx context.Context,
	controlPlane *client.Client,
	directory bundle.Directory,
	scope client.EnvironmentScopeOptions,
	requirePromotion bool,
	reporter deployReporter,
) error {
	uploads, err := planDeploymentBundleObjectUploads(ctx, controlPlane, directory, scope, requirePromotion)
	if err != nil {
		return err
	}
	reconciled := false
	totalUploads := len(uploads)
	completedUploads := 0
	for len(uploads) > 0 {
		// Keep a small, bounded batch. Join every request before reconciliation or
		// returning, so no upload can still use the temporary bundle after cleanup.
		batch := uploads[:min(2, len(uploads))]
		uploadErrors, reportErr := uploadDeploymentBundleBatch(ctx, controlPlane, directory, scope, batch, completedUploads, totalUploads, reporter)
		var originalErr error
		unattempted := false
		for index, uploadErr := range uploadErrors {
			if uploadErr != nil {
				originalErr = errors.Join(originalErr, fmt.Errorf("upload deployment object %s: %w", batch[index].Digest, uploadErr))
				unattempted = unattempted || errors.Is(uploadErr, client.ErrDeploymentObjectUploadNotAttempted)
			}
		}
		if reportErr != nil || ctx.Err() != nil {
			return errors.Join(originalErr, reportErr, ctx.Err())
		}
		if originalErr == nil {
			completedUploads += len(batch)
			uploads = uploads[len(batch):]
			continue
		}
		if reconciled || unattempted {
			return originalErr
		}
		reconciled = true
		if err := reporter.Step("Reconciling deployment upload"); err != nil {
			return errors.Join(originalErr, err)
		}
		replanned, planErr := planDeploymentBundleObjectUploads(ctx, controlPlane, directory, scope, requirePromotion)
		if planErr != nil {
			return errors.Join(originalErr, fmt.Errorf("reconcile deployment upload: %w", planErr))
		}
		for _, candidate := range replanned {
			for index, upload := range batch {
				if uploadErrors[index] != nil && candidate.Digest == upload.Digest {
					return originalErr
				}
			}
		}
		for index, upload := range batch {
			if uploadErrors[index] != nil {
				if err := reporter.DeploymentObjectUploaded(upload.Digest, completedUploads+index+1, totalUploads); err != nil {
					return errors.Join(originalErr, err)
				}
			}
		}
		completedUploads += len(batch)
		uploads = replanned
	}
	return nil
}

func uploadDeploymentBundleBatch(
	ctx context.Context, controlPlane *client.Client, directory bundle.Directory,
	scope client.EnvironmentScopeOptions,
	uploads []api.DeploymentBundleUpload, completed, total int, reporter deployReporter,
) ([]error, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reports sync.Mutex
	var reportErr error
	// Reporters may write to a non-concurrent writer. Preserve whole JSON lines
	// and stop all transfers if the output consumer fails.
	report := func(write func() error) error {
		reports.Lock()
		defer reports.Unlock()
		if reportErr == nil {
			reportErr = write()
			if reportErr != nil {
				cancel()
			}
		}
		return reportErr
	}
	errs := make([]error, len(uploads))
	var workers sync.WaitGroup
	for offset, upload := range uploads {
		workers.Go(func() {
			index := completed + offset + 1
			if err := report(func() error { return reporter.DeploymentObjectUploadStarted(upload.Digest, index, total) }); err != nil {
				return
			}
			errs[offset] = controlPlane.UploadDeploymentBundleObject(ctx, directory.Digest, scope, upload, directory.Objects[upload.Digest],
				func(read, size int64) error {
					return report(func() error { return reporter.DeploymentObjectUploadProgress(upload.Digest, index, total, read, size) })
				})
			if errs[offset] == nil {
				_ = report(func() error { return reporter.DeploymentObjectUploaded(upload.Digest, index, total) })
			}
		})
	}
	workers.Wait()
	return errs, reportErr
}

func planDeploymentBundleObjectUploads(
	ctx context.Context,
	controlPlane *client.Client,
	directory bundle.Directory,
	scope client.EnvironmentScopeOptions,
	requirePromotion bool,
) ([]api.DeploymentBundleUpload, error) {
	plan, err := controlPlane.PlanDeploymentBundleUploads(ctx, directory.BundleJSON, scope, requirePromotion)
	if err != nil {
		return nil, err
	}
	if plan.BundleDigest != directory.Digest {
		return nil, errors.New("deployment upload plan returned a different bundle digest")
	}
	seen := make(map[string]struct{}, len(plan.Uploads))
	for _, upload := range plan.Uploads {
		if _, duplicate := seen[upload.Digest]; duplicate {
			return nil, errors.New("deployment upload plan contains a duplicate object")
		}
		seen[upload.Digest] = struct{}{}
		if _, ok := directory.Objects[upload.Digest]; !ok {
			return nil, errors.New("deployment upload plan requested an object outside the bundle")
		}
		matched := false
		for _, object := range directory.Bundle.Objects {
			if object.Digest == upload.Digest && object.SizeBytes == upload.SizeBytes && object.MediaType == upload.MediaType {
				matched = true
				break
			}
		}
		if !matched {
			return nil, errors.New("deployment upload plan descriptor differs from the bundle")
		}
	}
	return plan.Uploads, nil
}

type deployReporter interface {
	Step(string) error
	DeploymentObjectUploadStarted(digest string, index int, count int) error
	DeploymentObjectUploadProgress(digest string, index int, count int, bytesRead int64, totalBytes int64) error
	DeploymentObjectUploaded(digest string, index int, count int) error
	DeploymentObjectVerified(api.DeploymentBundleFinalizeObject) error
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
	Digest     string                  `json:"digest,omitempty"`
	Index      int                     `json:"index,omitempty"`
	Count      int                     `json:"count,omitempty"`
	BytesRead  int64                   `json:"bytes_read,omitempty"`
	TotalBytes int64                   `json:"total_bytes,omitempty"`
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

func (r cliDeployReporter) DeploymentObjectUploadStarted(digest string, index int, count int) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{
			Type: "deployment_object_upload_started", Digest: digest, Index: index, Count: count,
		}})
	}
	_, err := fmt.Fprintf(
		r.cmd.ErrOrStderr(),
		"Uploading deployment object %s (%d/%d)\n",
		digest,
		index,
		count,
	)
	return err
}

func (r cliDeployReporter) DeploymentObjectUploadProgress(
	digest string,
	index int,
	count int,
	bytesRead int64,
	totalBytes int64,
) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{
			Type: "deployment_object_upload_progress", Digest: digest, Index: index, Count: count,
			BytesRead: bytesRead, TotalBytes: totalBytes,
		}})
	}
	_, err := fmt.Fprintf(
		r.cmd.ErrOrStderr(),
		"Deployment object %s transfer: %d/%d bytes (%d/%d)\n",
		digest,
		bytesRead,
		totalBytes,
		index,
		count,
	)
	return err
}

func (r cliDeployReporter) DeploymentObjectUploaded(digest string, index int, count int) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{
			Type: "deployment_object_uploaded", Digest: digest, Index: index, Count: count,
		}})
	}
	_, err := fmt.Fprintf(
		r.cmd.ErrOrStderr(),
		"Deployment object %s uploaded (%d/%d)\n",
		digest,
		index,
		count,
	)
	return err
}

func (r cliDeployReporter) DeploymentObjectVerified(value api.DeploymentBundleFinalizeObject) error {
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{
			Type: "deployment_object_verified", Digest: value.Digest,
		}})
	}
	_, err := fmt.Fprintf(r.cmd.ErrOrStderr(), "Deployment object %s verified\n", value.Digest)
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
	if phase == "promoted" && !r.jsonOutput {
		fmt.Fprintln(r.cmd.ErrOrStderr(), promotionEffects)
	}
	if r.jsonOutput {
		return writeJSONLines(r.cmd.OutOrStdout(), []cliDeployLine{{Type: "deployment_result", Phase: phase, Deployment: &value}})
	}
	_, err := fmt.Fprintln(r.cmd.OutOrStdout(), deploymentOutputRef(value))
	return err
}

const promotionEffects = "New Sessions and future triggers now use this Deployment. Agents absent from it no longer accept new independent Sessions or Slack conversations; retained identities, connections and pinned Sessions are unchanged. Inactive trigger intervals are not replayed."

func promoteCommand() *cobra.Command {
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "promote DEPLOYMENT",
		Short: "Promote an immutable deployment to current.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			value, err := controlPlane.PromoteDeployment(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), deploymentOutputRef(value))
			fmt.Fprintln(cmd.ErrOrStderr(), promotionEffects)
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project ID or slug for the deployment.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment ID or slug for the deployment.")
	return cmd
}

func deploymentOutputRef(value api.DeploymentResponse) string {
	if strings.TrimSpace(value.Version) != "" {
		return strings.TrimSpace(value.Version)
	}
	return value.ID
}
