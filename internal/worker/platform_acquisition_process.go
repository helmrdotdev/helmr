package worker

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	Candidates *workerapi.PlatformAcquisitionCandidates   `json:"candidates,omitempty"`
	Error      string                                     `json:"error,omitempty"`
	Reason     workerapi.PlatformAcquisitionFailureReason `json:"reason,omitempty"`
}

type platformAcquisitionProcessError struct {
	cause  error
	reason workerapi.PlatformAcquisitionFailureReason
}

func (err *platformAcquisitionProcessError) Error() string { return err.cause.Error() }
func (err *platformAcquisitionProcessError) Unwrap() error { return err.cause }
func (err *platformAcquisitionProcessError) PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason {
	return err.reason
}

func (process PlatformAcquisitionProcess) Acquire(
	ctx context.Context,
	request workerapi.PlatformAcquisition,
) (workerapi.PlatformAcquisitionCandidates, error) {
	if ctx == nil {
		return workerapi.PlatformAcquisitionCandidates{}, errors.New("platform acquisition context is nil")
	}
	bounded, cancel := context.WithTimeout(ctx, platformAcquisitionDeadline)
	defer cancel()
	result, err := runPlatformAcquisitionProcess(bounded, process, request)
	if err != nil {
		return workerapi.PlatformAcquisitionCandidates{}, err
	}
	if result.Candidates != nil && result.Error == "" && result.Reason == "" {
		return *result.Candidates, nil
	}
	if result.Candidates != nil || result.Error == "" {
		return workerapi.PlatformAcquisitionCandidates{}, errors.New("platform acquisition child returned an invalid result")
	}
	cause := errors.New(result.Error)
	if result.Reason == "" {
		return workerapi.PlatformAcquisitionCandidates{}, cause
	}
	return workerapi.PlatformAcquisitionCandidates{}, &platformAcquisitionProcessError{
		cause:  cause,
		reason: result.Reason,
	}
}
