package computerhost

import (
	"context"

	"errors"
	"fmt"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"

	"log/slog"

	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"

	"github.com/helmrdotdev/helmr/internal/ids"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// PreparedMachines supplies immutable artifacts and tracked devices to allocation owners.
type PreparedMachines struct {
	CommandLogLimits     *computerv0.CommandLogLimits
	SessionLogLimits     *agentv1.SessionLogLimits
	PreparationLogLimits *computerv0.PreparationLogLimits
	Backend              vm.Backend
	CAS                  cas.Store
	ComputerObjects      cas.ImmutableStore
	CheckpointCipher     *CheckpointEncryptor
	ComputerRanges       blockformat.RangeSource
	ComputerHelper       string
	ComputerDevices      []string
	ComputerStagingBytes int64
	ComputerSaveEvery    time.Duration
	TempDir              string
	Log                  *slog.Logger
	Reservations         *reservation.Ledger
	PlatformStore        cas.Reader
	RuntimeArchitecture  definition.RuntimeArchitecture
	VerifierCgroupRoot   string

	mu                sync.Mutex
	computerDevices   map[preparedMachineRef]vm.ComputerDevice
	verifiedRuntimes  map[artifact.RuntimeDescriptor]artifact.RuntimeIndex
	programDescriptor artifact.ProgramDescriptor
	programMetadata   *artifact.ProgramMetadata
}

type preparedMachineRef struct {
	id    string
	epoch int64
}

func NewPreparedMachines(backend vm.Backend, store cas.Store, log *slog.Logger) *PreparedMachines {
	return &PreparedMachines{Backend: backend, CAS: store, Log: log}
}

type programSnapshots struct {
	runtime  *snapshot.Runtime
	artifact *snapshot.Program
}

func (s *programSnapshots) close() error {
	return errors.Join(s.runtime.Close(), s.artifact.Close())
}

func (p *PreparedMachines) prepareProgramArtifacts(ctx context.Context, tempDir, instanceID string, program *workerapi.RuntimeProgram) (*programSnapshots, error) {
	if strings.TrimSpace(program.DeploymentID) == "" {
		return nil, errors.New("program deployment id is required")
	}
	if p.PlatformStore == nil {
		return nil, errors.New("managed runtime delivery is not configured")
	}
	runtimeDescriptor := artifact.RuntimeDescriptor{
		Architecture:    p.RuntimeArchitecture,
		Digest:          program.Runtime.Digest,
		FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
		MediaType:       program.Runtime.MediaType,
		RuntimeContract: definition.RuntimeContract,
		SizeBytes:       program.Runtime.SizeBytes,
	}
	runtimeSnapshot, err := snapshot.ReadRuntime(
		ctx,
		p.PlatformStore,
		tempDir,
		runtimeDescriptor,
	)
	if err != nil {
		return nil, err
	}
	closeSnapshots := func() error { return runtimeSnapshot.Close() }
	started := time.Now()
	runtimeIndex, memoHit, err := p.verifyRuntime(runtimeDescriptor, func() (artifact.RuntimeIndex, error) {
		return verify.Runtime(
			ctx,
			p.VerifierCgroupRoot,
			instanceID,
			runtimeSnapshot,
		)
	})
	p.logInfo("artifact verified",
		"verification_id", instanceID,
		"duration_ms", time.Since(started).Milliseconds(),
		"memo_hit", memoHit,
		"error", errorString(err),
	)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("verify managed runtime: %w", err),
			closeSnapshots(),
		)
	}
	expectedRuntimeIndex := artifact.RuntimeIndex{
		Architecture:    runtimeDescriptor.Architecture,
		RuntimeContract: runtimeDescriptor.RuntimeContract,
	}
	if runtimeIndex != expectedRuntimeIndex {
		return nil, errors.Join(
			errors.New("managed runtime index does not match its descriptor"),
			closeSnapshots(),
		)
	}
	programDescriptor := artifact.ProgramDescriptor{
		Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType,
	}
	programSnapshot, err := snapshot.ReadProgram(
		ctx,
		p.CAS,
		tempDir,
		programDescriptor,
	)
	if err != nil {
		return nil, errors.Join(err, closeSnapshots())
	}
	closeSnapshots = func() error {
		return errors.Join(runtimeSnapshot.Close(), programSnapshot.Close())
	}
	programMetadata, err := p.verifyProgram(ctx, programDescriptor, func() (artifact.ProgramMetadata, error) {
		return verify.Program(ctx, p.VerifierCgroupRoot, instanceID, programSnapshot)
	})
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("verify program: %w", err),
			closeSnapshots(),
		)
	}
	if programMetadata.RuntimeDigest != runtimeDescriptor.Digest ||
		programMetadata.RuntimeContract != runtimeDescriptor.RuntimeContract ||
		programMetadata.Architecture != runtimeDescriptor.Architecture {
		return nil, errors.Join(
			errors.New("program metadata does not match runtime reservation authority"),
			closeSnapshots(),
		)
	}
	if err := verifyProgramMetadataDigest(programMetadata, program.IndexDigest); err != nil {
		return nil, errors.Join(
			err,
			closeSnapshots(),
		)
	}

	return &programSnapshots{runtime: runtimeSnapshot, artifact: programSnapshot}, nil
}

// Callers must verify a fresh snapshot against descriptor before every call,
// including memo hits.
func (p *PreparedMachines) verifyProgram(
	ctx context.Context,
	descriptor artifact.ProgramDescriptor,
	verify func() (artifact.ProgramMetadata, error),
) (artifact.ProgramMetadata, error) {
	if err := ctx.Err(); err != nil {
		return artifact.ProgramMetadata{}, err
	}
	p.mu.Lock()
	cached := p.programMetadata
	if p.programDescriptor != descriptor {
		cached = nil
	}
	p.mu.Unlock()
	if cached != nil {
		return cached.Clone(), nil
	}
	index, err := verify()
	if err != nil {
		return artifact.ProgramMetadata{}, err
	}
	owned := index.Clone()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return artifact.ProgramMetadata{}, err
	}
	p.programDescriptor = descriptor
	p.programMetadata = &owned
	return index, nil
}

func (p *PreparedMachines) verifyRuntime(
	descriptor artifact.RuntimeDescriptor,
	verify func() (artifact.RuntimeIndex, error),
) (artifact.RuntimeIndex, bool, error) {
	p.mu.Lock()
	index, ok := p.verifiedRuntimes[descriptor]
	p.mu.Unlock()
	if ok {
		return index, true, nil
	}
	index, err := verify()
	if err != nil {
		return artifact.RuntimeIndex{}, false, err
	}

	p.mu.Lock()
	if p.verifiedRuntimes == nil {
		p.verifiedRuntimes = make(map[artifact.RuntimeDescriptor]artifact.RuntimeIndex)
	}
	p.verifiedRuntimes[descriptor] = index
	p.mu.Unlock()
	return index, false, nil
}

func verifyProgramMetadataDigest(
	index artifact.ProgramMetadata,
	expectedDigest string,
) error {
	indexBytes, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		return fmt.Errorf("canonicalize verified program metadata: %w", err)
	}
	if sha256sum.DigestBytes(indexBytes) != expectedDigest {
		return errors.New("program metadata does not match deployment authority")
	}
	return nil
}

func (p *PreparedMachines) releaseInstanceCapacity(computerInstanceID string, workerEpoch int64) error {
	if err := p.releaseComputerDevice(computerInstanceID, workerEpoch); err != nil {
		return err
	}
	if p == nil || p.Reservations == nil {
		return nil
	}
	if ids.Validate(computerInstanceID) == nil && workerEpoch > 0 {
		if err := os.RemoveAll(p.computerPreparationDirectory(computerInstanceID, workerEpoch)); err != nil {
			return err
		}
	}
	if err := p.Reservations.Release(instanceReservationKey(computerInstanceID, workerEpoch)); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.computerDevices, preparedMachineRef{id: computerInstanceID, epoch: workerEpoch})
	p.mu.Unlock()
	return nil
}

func (p *PreparedMachines) logInfo(message string, attrs ...any) {
	if p == nil || p.Log == nil {
		return
	}
	p.Log.Info(message, attrs...)
}
