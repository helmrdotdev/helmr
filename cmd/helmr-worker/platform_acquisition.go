package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const platformAcquisitionInputBytes = 64 << 10

func runPlatformAcquisitionChild(
	ctx context.Context,
	arguments []string,
) (bool, error) {
	if len(arguments) < 2 || arguments[1] != "__platform-acquire" {
		return false, nil
	}
	if len(arguments) != 2 {
		return true, errors.New("platform acquisition child arguments are invalid")
	}
	var request workerapi.PlatformAcquisition
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, platformAcquisitionInputBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return true, fmt.Errorf("decode platform acquisition request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return true, errors.New("platform acquisition request has trailing JSON")
	}
	workDir := os.Getenv("HELMR_PLATFORM_ACQUISITION_WORK_DIR")
	policy, err := deployment.LoadBuildPolicy(
		os.Getenv("HELMR_PLATFORM_ACQUISITION_BUILD_POLICY"),
	)
	if err != nil {
		return true, fmt.Errorf("load platform acquisition policy: %w", err)
	}
	store, err := cass3.NewImmutable(
		ctx,
		os.Getenv("HELMR_PLATFORM_ACQUISITION_STORE"),
		cass3.WithTempDir(workDir),
	)
	if err != nil {
		return true, fmt.Errorf("open platform acquisition store: %w", err)
	}
	acquirer := deployment.PlatformAcquirer{
		Encoder: os.Getenv("HELMR_PLATFORM_ACQUISITION_ENCODER"),
		GPGV:    os.Getenv("HELMR_PLATFORM_ACQUISITION_GPGV"),
		Policy:  policy,
		Store:   store,
		Validator: deployment.ConformanceValidator{
			UnitCgroupRoot: os.Getenv("HELMR_PLATFORM_ACQUISITION_UNIT_CGROUP"),
		},
		WorkDir: workDir,
		XZ:      os.Getenv("HELMR_PLATFORM_ACQUISITION_XZ"),
	}
	candidates, acquisitionErr := acquirer.Acquire(ctx, request)
	result := worker.PlatformAcquisitionProcessResult{}
	if acquisitionErr == nil {
		result.Candidates = &candidates
	} else {
		result.Error = acquisitionErr.Error()
		var deterministic interface {
			PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason
		}
		if errors.As(acquisitionErr, &deterministic) {
			result.Reason = deterministic.PlatformAcquisitionFailureReason()
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return true, fmt.Errorf("encode platform acquisition result: %w", err)
	}
	return true, nil
}
