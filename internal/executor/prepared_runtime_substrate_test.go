package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sys/unix"
)

func TestRuntimeSubstrateCacheSourceProjectsWithoutMutatingCacheAuthority(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "small", body: []byte("immutable substrate bytes")},
		{name: "dense", body: bytes.Repeat([]byte{0x5a}, (96<<10)+13)},
		{name: "nonzero chunk tails", body: bytes.Repeat(append(make([]byte, (32<<10)-1), 1), 3)},
		{name: "all zero with partial tail", body: make([]byte, (96<<10)+13)},
		{name: "leading zeros", body: append(make([]byte, 64<<10), []byte("content")...)},
		{name: "trailing zeros", body: append([]byte("content"), make([]byte, (96<<10)+13)...)},
		{name: "mixed unaligned spans", body: bytes.Join([][]byte{
			[]byte("start"), make([]byte, (64<<10)+7), []byte("middle"),
			make([]byte, (64<<10)+19), []byte("end"),
		}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cachePath := t.TempDir() + "/substrate.ext4"
			if err := os.WriteFile(cachePath, test.body, 0o444); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(test.body)
			source := &runtimeSubstrateCacheSource{
				path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(test.body)),
			}
			projected, err := source.MaterializeInto(
				context.Background(), t.TempDir(), "substrate.ext4", os.Getuid(), os.Getgid(),
			)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("cache authority changed: before=%v after=%v", before, after)
			}
			if got, err := os.ReadFile(projected); err != nil || !bytes.Equal(got, test.body) {
				t.Fatalf("projected substrate differs: length=%d, err=%v", len(got), err)
			}
			projectedInfo, err := os.Stat(projected)
			if err != nil {
				t.Fatal(err)
			}
			if projectedInfo.Mode().Perm() != 0o440 || os.SameFile(after, projectedInfo) {
				t.Fatalf("projection must be sealed and have its own inode: %v", projectedInfo)
			}
			var stat unix.Stat_t
			if err := unix.Stat(projected, &stat); err != nil {
				t.Fatal(err)
			}
			if stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
				t.Fatalf("projection owner = %d:%d", stat.Uid, stat.Gid)
			}
		})
	}
}

func TestRuntimeSubstrateProjectionFailureLeavesNoPartialFile(t *testing.T) {
	for _, kind := range []string{"cancelled", "digest mismatch", "size changed", "file collision", "symlink collision"} {
		t.Run(kind, func(t *testing.T) {
			body := make([]byte, (64<<10)+7)
			cachePath := t.TempDir() + "/substrate.ext4"
			if err := os.WriteFile(cachePath, body, 0o444); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			source := &runtimeSubstrateCacheSource{path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(body))}
			arena := t.TempDir()
			destination := arena + "/substrate.ext4"
			ctx := context.Background()
			switch kind {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "digest mismatch":
				source.digest = "sha256:" + strings.Repeat("0", 64)
			case "size changed":
				source.sizeBytes++
			case "file collision":
				if err := os.WriteFile(destination, []byte("existing file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink collision":
				if err := os.Symlink(cachePath, destination); err != nil {
					t.Fatal(err)
				}
			}
			path, err := source.MaterializeInto(ctx, arena, "substrate.ext4", os.Getuid(), os.Getgid())
			if err == nil || path != "" {
				t.Fatalf("projection = %q, err=%v", path, err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
			switch kind {
			case "file collision":
				if got, err := os.ReadFile(destination); err != nil || string(got) != "existing file" {
					t.Fatalf("existing file changed: %q, %v", got, err)
				}
			case "symlink collision":
				if target, err := os.Readlink(destination); err != nil || target != cachePath {
					t.Fatalf("existing symlink changed: %q, %v", target, err)
				}
			default:
				if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial projection remains: %v", err)
				}
			}
			if got, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(got, body) {
				t.Fatal("source was modified")
			}
		})
	}
}

func TestSubstrateProjectionWriterPropagatesFileErrors(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writer := substrateProjectionWriter{file: file}
	for _, body := range [][]byte{{0}, {1}} {
		if n, err := writer.Write(body); n != 0 || !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed file write = %d, %v", n, err)
		}
	}
}

func TestRuntimeSubstrateProjectionKeepsZeroSpansSparseOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("allocation assertion requires Linux")
	}
	arena := t.TempDir()
	var filesystem unix.Statfs_t
	if err := unix.Statfs(arena, &filesystem); err != nil {
		t.Fatal(err)
	}
	const ext4Magic, tmpfsMagic = 0xef53, 0x01021994
	if filesystem.Type != ext4Magic && filesystem.Type != tmpfsMagic {
		t.Skip("allocation assertion requires an ext4 or tmpfs test directory")
	}
	body := make([]byte, (8<<20)+7)
	body[0], body[len(body)-1] = 1, 2
	cachePath := arena + "/dense.ext4"
	if err := os.WriteFile(cachePath, body, 0o444); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	source := &runtimeSubstrateCacheSource{path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(body))}
	projected, err := source.MaterializeInto(context.Background(), arena, "substrate.ext4", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(projected, &stat); err != nil {
		t.Fatal(err)
	}
	var dense unix.Stat_t
	if err := unix.Stat(cachePath, &dense); err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem=%#x logical=%d dense allocated=%d sparse allocated=%d", filesystem.Type, stat.Size, dense.Blocks*512, stat.Blocks*512)
	if dense.Size != int64(len(body)) || dense.Blocks*512 < dense.Size {
		t.Fatalf("dense control was not fully allocated: size=%d, allocated=%d", dense.Size, dense.Blocks*512)
	}
	if stat.Size != dense.Size || stat.Blocks*512 >= dense.Blocks*512/2 {
		t.Fatalf("zero spans were allocated: size=%d, allocated=%d", stat.Size, stat.Blocks*512)
	}
	if got, err := os.ReadFile(projected); err != nil || !bytes.Equal(got, body) {
		t.Fatal("sparse output contents differ")
	}
}

func TestRuntimeCapacityChargesArenaSubstrateProjection(t *testing.T) {
	request, err := runtimeCapacityVectorWithProjection(1000, 512, 1024, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1280 << 20); request.GuestEphemeralDiskBytes != want {
		t.Fatalf("arena disk reservation = %d, want %d", request.GuestEphemeralDiskBytes, want)
	}
}

func TestPreparedRuntimeRestoreRebuildsAndRegistersSubstrateWithoutSubstrateCAS(t *testing.T) {
	store, fixture := testWorkspaceMountArtifacts(t)
	target := workerapi.RuntimeReconcileTarget{
		Source: workerapi.RuntimeSource{
			WorkspaceID:            "019c10d5-a6f7-7af1-8f5f-000000000801",
			DeploymentDefinitionID: "019c10d5-a6f7-7af1-8f5f-000000000802",
			WorkspaceTarget: &workerapi.WorkspaceResetTarget{
				BaseWorkspaceVersionID: "019c10d5-a6f7-7af1-8f5f-000000000803",
			},
			WorkspaceImage: fixture.WorkspaceImage,
			Restore:        &workerapi.RuntimeRestore{CheckpointID: "checkpoint-1"},
		},
	}
	substratePath := t.TempDir() + "/substrate.ext4"
	if err := os.WriteFile(substratePath, []byte("rebuilt substrate"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := substrate.CacheKey(substrate.Source{WorkspaceImageDigest: fixture.WorkspaceImage.Digest, WorkspaceImageMediaType: fixture.WorkspaceImage.MediaType})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &restoreSubstrateResolver{
		result: substrate.Result{
			CacheKey:  key,
			Path:      substratePath,
			Digest:    "sha256:" + strings.Repeat("a", 64),
			Format:    substrate.Format,
			Contract:  substrate.Contract,
			SizeBytes: int64(len("rebuilt substrate")),
		},
	}
	pool := &PreparedRuntimePool{Substrates: resolver}
	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	_, cleanup, topology, err := pool.restoreWorkspaceImageAndRuntimeSubstrate(
		context.Background(),
		WorkspaceMaterializer{CAS: store},
		t.TempDir(),
		mount,
		"restore image",
		"restore substrate",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if resolver.calls != 1 ||
		resolver.source.WorkspaceImageDigest != target.Source.WorkspaceImage.Digest ||
		resolver.source.WorkspaceImageMediaType != target.Source.WorkspaceImage.MediaType {
		t.Fatalf("resolver calls = %d, source = %+v", resolver.calls, resolver.source)
	}
	if topology.Substrate == nil ||
		topology.Substrate.Digest != resolver.result.Digest ||
		topology.Substrate.SizeBytes != resolver.result.SizeBytes {
		t.Fatalf("topology = %+v, want rebuilt substrate", topology)
	}

	registrar := &immutableSubstrateRegistrar{}
	registered, err := registerRuntimeSubstrate(
		context.Background(),
		registrar,
		target.Source.DeploymentDefinitionID,
		topology.Substrate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ID != registrar.id ||
		registrar.request.SubstrateDigest != resolver.result.Digest ||
		registrar.request.Format != substrate.Format ||
		registrar.request.Contract != substrate.Contract ||
		registrar.request.SizeBytes != resolver.result.SizeBytes {
		t.Fatalf("registration = %+v, request = %+v", registered, registrar.request)
	}
	if got := store.getCalls[target.Source.WorkspaceImage.Digest]; got != 1 {
		t.Fatalf("Workspace image CAS reads = %d, want 1", got)
	}
	if len(store.getCalls) != 1 {
		t.Fatalf("CAS reads = %+v, want only the Workspace image", store.getCalls)
	}

	conflicting := *topology.Substrate
	conflicting.Digest = "sha256:" + strings.Repeat("b", 64)
	if _, err := registerRuntimeSubstrate(
		context.Background(),
		registrar,
		target.Source.DeploymentDefinitionID,
		&conflicting,
	); !errors.Is(err, errRuntimeSubstrateConflict) {
		t.Fatalf("conflicting registration error = %v, want conflict", err)
	}
}

type restoreSubstrateResolver struct {
	calls  int
	source substrate.Source
	result substrate.Result
}

func (r *restoreSubstrateResolver) Resolve(
	_ context.Context,
	imagePath string,
	source substrate.Source,
) (substrate.Result, error) {
	image, err := os.ReadFile(imagePath)
	if err != nil {
		return substrate.Result{}, err
	}
	if string(image) != "oci image" {
		return substrate.Result{}, errors.New("unexpected workspace image")
	}
	r.calls++
	r.source = source
	return r.result, nil
}

var errRuntimeSubstrateConflict = errors.New("runtime substrate conflict")

type immutableSubstrateRegistrar struct {
	id      string
	request workerapi.RuntimeSubstrateRegisterRequest
}

func (r *immutableSubstrateRegistrar) RegisterRuntimeSubstrate(
	_ context.Context,
	request workerapi.RuntimeSubstrateRegisterRequest,
) (workerapi.RuntimeSubstrateRegisterResponse, error) {
	if r.id == "" {
		r.id = "019c10d5-a6f7-7af1-8f5f-000000000804"
		r.request = request
	} else if request.DeploymentDefinitionID != r.request.DeploymentDefinitionID ||
		request.Format != r.request.Format ||
		request.Contract != r.request.Contract ||
		request.SubstrateDigest != r.request.SubstrateDigest ||
		request.SizeBytes != r.request.SizeBytes {
		return workerapi.RuntimeSubstrateRegisterResponse{}, errRuntimeSubstrateConflict
	}
	return workerapi.RuntimeSubstrateRegisterResponse{
		RuntimeSubstrate: workerapi.RuntimeSubstrate{
			ID:                     r.id,
			DeploymentDefinitionID: request.DeploymentDefinitionID,
			SubstrateDigest:        request.SubstrateDigest,
			Format:                 request.Format,
			Contract:               request.Contract,
			SizeBytes:              request.SizeBytes,
		},
	}, nil
}

var _ RuntimeSubstrateResolver = (*restoreSubstrateResolver)(nil)
var _ RuntimeSubstrateRegistrar = (*immutableSubstrateRegistrar)(nil)

func TestRuntimeSubstrateRejectsDifferentImageIdentity(t *testing.T) {
	_, mount := testWorkspaceMountArtifacts(t)
	path := t.TempDir() + "/image.tar"
	if err := os.WriteFile(path, []byte("oci image"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := &restoreSubstrateResolver{result: substrate.Result{CacheKey: "wrong-image"}}
	topology, err := runtimeSubstrateTopology(context.Background(), resolver, path, mount)
	if err == nil || topology.Substrate != nil {
		t.Fatalf("topology = %v, err = %v", topology, err)
	}
}
