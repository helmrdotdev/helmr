package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

const (
	waitMetadataJSONMaxBytes = 64 * 1024
	waitTagsMaxCount         = 32
	waitTagMaxBytes          = 128
)

// MountRegistry is the Run side's view of mounted Computers: it borrows a
// Run's channel, asks the physical owner to fail a mount, and renews the
// Computer authority installed in the guest.
type MountRegistry interface {
	OpenChannel(context.Context, string) (computerhost.MountChannel, error)
	RequestFailure(context.Context, string) error
	RenewComputerAuthority(context.Context, *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error)
}

type ProgramRunner struct {
	ControlPlane     ControlPlane
	ComputerCaptures *computerhost.CaptureRuns
	CAS              cas.Store
	Mounts           MountRegistry
	Log              *slog.Logger
	TempDir          string
}

// NewProgramRunner validates the runner's required collaborators so that
// incomplete wiring is rejected before any run lease is claimed.
func NewProgramRunner(runner ProgramRunner) (ProgramRunner, error) {
	if err := runner.validate(); err != nil {
		return ProgramRunner{}, err
	}
	return runner, nil
}

func (r ProgramRunner) validate() error {
	if err := r.ControlPlane.Validate(); err != nil {
		return err
	}
	if r.CAS == nil {
		return errors.New("run lease task CAS is required")
	}
	if r.ComputerCaptures == nil {
		return errors.New("run lease task Computer capture registry is required")
	}
	if r.Mounts == nil {
		return errors.New("computer mount session registry is required")
	}
	return nil
}

func readResumeAck(ctx context.Context, machine vm.Machine) (*programv0.ResumeAck, error) {
	var ack programv0.ResumeAck
	if err := readProtoFrameContext(ctx, machine, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func readProtoFrameContext(
	ctx context.Context,
	machine vm.Machine,
	message proto.Message,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrame(machine.Stream(), message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = machine.Close(context.Background())
		return ctx.Err()
	}
}

func readProtoFrameBoundedContext(
	ctx context.Context,
	machine vm.Machine,
	maxBytes uint32,
	message proto.Message,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrameBounded(machine.Stream(), maxBytes, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = machine.Close(context.Background())
		return ctx.Err()
	}
}

func parseWaitRequest(
	leases workerapi.RunLeaseProvider,
	wait *programv0.RunWaitRequested,
) (WaitRequest, error) {
	if leases == nil {
		return WaitRequest{}, errors.New("run lease provider is required")
	}
	if wait == nil {
		return WaitRequest{}, errors.New("guest wait request is empty")
	}
	correlationID := wait.GetCorrelationId()
	if err := ids.Validate(correlationID); err != nil {
		return WaitRequest{}, errors.New("guest wait request correlation_id must be a canonical UUIDv7")
	}
	runWaitID := wait.GetRunWaitId()
	if err := ids.Validate(runWaitID); err != nil {
		return WaitRequest{}, errors.New("guest wait request run_wait_id must be a canonical UUIDv7")
	}
	resumeAttachID := wait.GetResumeAttachId()
	if err := ids.Validate(resumeAttachID); err != nil {
		return WaitRequest{}, errors.New("guest wait request resume_attach_id must be a canonical UUIDv7")
	}
	kind := workerapi.RunWaitKind(strings.TrimSpace(wait.GetKind()))
	if kind == "" {
		return WaitRequest{}, errors.New("guest wait request kind is required")
	}
	paramsJSON := strings.TrimSpace(wait.GetParamsJson())
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return WaitRequest{}, errors.New("guest wait params_json must be valid JSON")
	}
	metadataJSON := strings.TrimSpace(wait.GetMetadataJson())
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	if !json.Valid([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New("guest wait metadata_json must be valid JSON")
	}
	var metadataCompact bytes.Buffer
	if err := json.Compact(&metadataCompact, []byte(metadataJSON)); err != nil {
		return WaitRequest{}, fmt.Errorf(
			"guest wait metadata_json must be valid JSON: %w",
			err,
		)
	}
	if metadataCompact.Len() > waitMetadataJSONMaxBytes {
		return WaitRequest{}, fmt.Errorf(
			"guest wait metadata_json is %d bytes, exceeds max %d",
			metadataCompact.Len(),
			waitMetadataJSONMaxBytes,
		)
	}
	if !waitMetadataJSONObject([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New(
			"guest wait metadata_json must be a JSON object",
		)
	}
	tags, err := normalizeRuntimeWaitTags(wait.GetTags())
	if err != nil {
		return WaitRequest{}, err
	}
	timeout, err := waitTimeoutMilliseconds(wait.TimeoutMs)
	if err != nil {
		return WaitRequest{}, err
	}
	idleTimeout, err := waitTimeoutMilliseconds(wait.IdleTimeoutMs)
	if err != nil {
		return WaitRequest{}, err
	}
	return WaitRequest{
		Execution: wait.GetExecution(), TurnID: wait.TurnId,
		Lease:                         leases.CurrentWorkerRunLease(),
		CorrelationID:                 correlationID,
		RunWaitID:                     runWaitID,
		ResumeAttachID:                resumeAttachID,
		Kind:                          kind,
		Params:                        []byte(paramsJSON),
		Metadata:                      []byte(metadataJSON),
		Tags:                          tags,
		TimeoutMS:                     timeout,
		IdleTimeoutMS:                 idleTimeout,
		ActorSpeculativeInputSequence: wait.ActorSpeculativeInputSequence,
	}, nil
}

func normalizeRuntimeWaitTags(tags []string) ([]string, error) {
	if len(tags) > waitTagsMaxCount {
		return nil, fmt.Errorf(
			"guest wait tags has %d entries, exceeds max %d",
			len(tags),
			waitTagsMaxCount,
		)
	}
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("guest wait tags must be non-empty")
		}
		if len([]byte(tag)) > waitTagMaxBytes {
			return nil, fmt.Errorf(
				"guest wait tag is %d bytes, exceeds max %d",
				len([]byte(tag)),
				waitTagMaxBytes,
			)
		}
		normalized = append(normalized, tag)
	}
	return normalized, nil
}

func waitMetadataJSONObject(value []byte) bool {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	return decoded != nil
}

func waitTimeoutMilliseconds(value *uint64) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	if *value > math.MaxInt64 {
		return nil, fmt.Errorf(
			"wait timeout %d exceeds max %d",
			*value,
			int64(math.MaxInt64),
		)
	}
	timeout := int64(*value)
	return &timeout, nil
}
