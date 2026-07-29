package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/ids"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

var (
	restoreAttachTimeout     = 30 * time.Second
	checkpointSuspendTimeout = 5 * time.Minute
)

const (
	waitMetadataJSONMaxBytes = 64 * 1024
	waitTagsMaxCount         = 32
	waitTagMaxBytes          = 128
)

type ProgramRunner struct {
	CAS                 cas.Store
	CheckpointEncryptor *checkpoint.Encryptor
	WorkspaceMounts     WorkspaceMountSessionRegistry
	RuntimeSubstrates   RuntimeSubstrateRegistrar
	TempDir             string
}

func (r ProgramRunner) tempDir() string {
	if strings.TrimSpace(r.TempDir) != "" {
		return r.TempDir
	}
	return os.TempDir()
}

type runtimePhaseCollector struct {
	mu     sync.Mutex
	phases []vm.RuntimePhase
}

func (c *runtimePhaseCollector) Record(phase vm.RuntimePhase) {
	if c == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases = append(c.phases, phase)
}

func (c *runtimePhaseCollector) Snapshot() []api.WorkerCheckpointPhase {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]api.WorkerCheckpointPhase, 0, len(c.phases))
	for _, phase := range c.phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func readResumeAck(ctx context.Context, session vm.Session) (*runv0.ResumeAck, error) {
	var ack runv0.ResumeAck
	if err := readProtoFrameContext(ctx, session, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			_ = os.Remove(path)
		}
	}
}

func readProtoFrameContext(
	ctx context.Context,
	session vm.Session,
	message proto.Message,
) error {
	return readProtoFrameFromReaderContext(ctx, session, session.Stream(), message)
}

func readProtoFrameBoundedContext(
	ctx context.Context,
	session vm.Session,
	maxBytes uint32,
	message proto.Message,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrameBounded(session.Stream(), maxBytes, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return ctx.Err()
	}
}

func readProtoFrameFromReaderContext(
	ctx context.Context,
	session vm.Session,
	reader io.Reader,
	message proto.Message,
) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrame(reader, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return ctx.Err()
	}
}

func parseWaitRequest(
	leases api.WorkerRunLeaseProvider,
	wait *runv0.RunWaitRequested,
) (WaitRequest, error) {
	if leases == nil {
		return WaitRequest{}, errors.New("Run Lease provider is required")
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
	kind := api.WorkerRunWaitKind(strings.TrimSpace(wait.GetKind()))
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
