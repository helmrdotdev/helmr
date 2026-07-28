package worker

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

const platformAcquisitionDeadline = 15 * time.Minute

type PlatformAcquisitionProcess struct {
	BuildPolicyPath  string
	Encoder          string
	Executable       string
	GPGV             string
	Patchelf         string
	PlatformStoreURI string
	UnitCgroupRoot   string
	WorkDir          string
	XZ               string
}

type PlatformAcquisitionProcessResult struct {
	Candidates *api.WorkerPlatformAcquisitionCandidates   `json:"candidates,omitempty"`
	Error      string                                     `json:"error,omitempty"`
	Reason     api.WorkerPlatformAcquisitionFailureReason `json:"reason,omitempty"`
}

type platformAcquisitionProcessError struct {
	cause  error
	reason api.WorkerPlatformAcquisitionFailureReason
}

func (err *platformAcquisitionProcessError) Error() string { return err.cause.Error() }
func (err *platformAcquisitionProcessError) Unwrap() error { return err.cause }
func (err *platformAcquisitionProcessError) PlatformAcquisitionFailureReason() api.WorkerPlatformAcquisitionFailureReason {
	return err.reason
}

func (process PlatformAcquisitionProcess) Acquire(
	ctx context.Context,
	request api.WorkerPlatformAcquisition,
) (api.WorkerPlatformAcquisitionCandidates, error) {
	if ctx == nil {
		return api.WorkerPlatformAcquisitionCandidates{}, errors.New("Platform acquisition context is nil")
	}
	bounded, cancel := context.WithTimeout(ctx, platformAcquisitionDeadline)
	defer cancel()
	result, err := runPlatformAcquisitionProcess(bounded, process, request)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	if result.Candidates != nil && result.Error == "" && result.Reason == "" {
		return *result.Candidates, nil
	}
	if result.Candidates != nil || result.Error == "" {
		return api.WorkerPlatformAcquisitionCandidates{}, errors.New("Platform acquisition child returned an invalid result")
	}
	cause := errors.New(result.Error)
	if result.Reason == "" {
		return api.WorkerPlatformAcquisitionCandidates{}, cause
	}
	return api.WorkerPlatformAcquisitionCandidates{}, &platformAcquisitionProcessError{
		cause:  cause,
		reason: result.Reason,
	}
}
