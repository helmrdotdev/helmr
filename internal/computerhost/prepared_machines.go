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
	"github.com/helmrdotdev/helmr/internal/artifact/localcopy"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/objectcache"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
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
	ObjectCache          *objectcache.Cache
	ComputerHelper       string
	ComputerDevices      []string
	ComputerStagingBytes int64
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
	runtime  *localcopy.Runtime
	artifact *localcopy.Program
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
	runtimeDescriptor := p.sessionRuntimeDescriptor(program.Runtime)
	programDescriptor := sessionProgramDescriptor(program.Artifact)
	images := &programSnapshots{}
	group, stageCtx := errgroup.WithContext(ctx)
	// Both files already fit in the caller's staging reservation. Their reads
	// are independent; join them before verification or releasing either file.
	group.Go(func() error {
		started := time.Now()
		var err error
		images.runtime, err = localcopy.ReadRuntime(stageCtx, p.ObjectCache.Reader("runtime", p.PlatformStore), tempDir, runtimeDescriptor)
		p.logInfo("program artifact staged", "verification_id", instanceID, "role", "runtime", "size_bytes", runtimeDescriptor.SizeBytes, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
		return err
	})
	group.Go(func() error {
		started := time.Now()
		var err error
		images.artifact, err = localcopy.ReadProgram(stageCtx, p.ObjectCache.Reader("program", p.CAS), tempDir, programDescriptor)
		p.logInfo("program artifact staged", "verification_id", instanceID, "role", "program", "size_bytes", programDescriptor.SizeBytes, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, errors.Join(err, images.close())
	}
	runtimeSnapshot, programSnapshot := images.runtime, images.artifact
	closeSnapshots := images.close
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
	started = time.Now()
	programMetadata, err := p.verifyProgram(ctx, programDescriptor, func() (artifact.ProgramMetadata, error) {
		return verify.Program(ctx, p.VerifierCgroupRoot, instanceID, programSnapshot)
	})
	p.logInfo("program artifact verified", "verification_id", instanceID, "role", "program", "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
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

	return images, nil
}

// The memo records isolated verification of exact content, not current launch
// authority. Any snapshot consumed later must still be bound to its descriptor.
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
