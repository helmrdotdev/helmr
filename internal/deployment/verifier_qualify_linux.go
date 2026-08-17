//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const verifierQualificationDeadline = 30 * time.Second

// QualifyArtifactVerifier proves that both exact verifier child paths can
// enter their production isolation and return a closed invalid-artifact result.
func QualifyArtifactVerifier(ctx context.Context, unitCgroupRoot, tempRoot string) error {
	if ctx == nil {
		return errors.New("artifact verifier qualification context is nil")
	}
	if !filepath.IsAbs(tempRoot) {
		return errors.New("artifact verifier qualification temp root is not absolute")
	}
	qualificationCtx, cancel := context.WithTimeout(ctx, verifierQualificationDeadline)
	defer cancel()
	directory, err := os.MkdirTemp(tempRoot, "verifier-qualification-")
	if err != nil {
		return fmt.Errorf("create artifact verifier qualification directory: %w", err)
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect artifact verifier qualification directory: %w", err)
	}

	for _, job := range []verifierJob{runtimeVerifierJob, programVerifierJob} {
		artifacts := make([]*os.File, 0, job.artifactCount())
		for index := range job.artifactCount() {
			path := filepath.Join(directory, fmt.Sprintf("%s-%d.invalid", job, index))
			if err := os.WriteFile(path, []byte("helmr verifier qualification\n"), 0o400); err != nil {
				closeVerifierQualificationArtifacts(artifacts)
				return fmt.Errorf("create %s verifier qualification artifact: %w", job, err)
			}
			artifact, err := os.Open(path)
			if err != nil {
				closeVerifierQualificationArtifacts(artifacts)
				return fmt.Errorf("open %s verifier qualification artifact: %w", job, err)
			}
			artifacts = append(artifacts, artifact)
		}
		result, runErr := runVerifierProcess(qualificationCtx, verifierProcessConfig{
			job:            job,
			unitCgroupRoot: unitCgroupRoot,
			leaseIdentity:  "qualification-" + string(job),
			artifacts:      artifacts,
		})
		closeErr := closeVerifierQualificationArtifacts(artifacts)
		if runErr != nil {
			return fmt.Errorf("qualify %s verifier process: %w", job, runErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if result.kind != verifierInvalid || result.diagnostic != job.invalidDiagnostic() {
			return fmt.Errorf("%s verifier qualification returned a non-invalid outcome", job)
		}
	}
	return nil
}

func closeVerifierQualificationArtifacts(artifacts []*os.File) error {
	var closeErr error
	for _, artifact := range artifacts {
		closeErr = errors.Join(closeErr, artifact.Close())
	}
	if closeErr != nil {
		return fmt.Errorf("close artifact verifier qualification inputs: %w", closeErr)
	}
	return nil
}
