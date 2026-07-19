package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestManagerStorePublishesAndRereadsAuthority(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	documents.order = trees.order
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, tree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)

	winner, err := store.Publish(ctx, selector, capsule, tree)
	if err != nil {
		t.Fatal(err)
	}
	if winner != capsule {
		t.Fatalf("winner = %#v, want %#v", winner, capsule)
	}
	wantOrder := []string{
		"tree:" + capsule.Tree.Digest,
		"document:v0/capsules/sha256/",
		"document:v0/claims/sha256/",
	}
	if len(*trees.order) != len(wantOrder) {
		t.Fatalf("publication order = %v", *trees.order)
	}
	for index, want := range wantOrder {
		if !strings.HasPrefix((*trees.order)[index], want) {
			t.Fatalf("publication order = %v", *trees.order)
		}
	}

	resolved, err := store.Resolve(ctx, selector)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != capsule {
		t.Fatalf("resolved = %#v, want %#v", resolved, capsule)
	}
}

func TestManagerStorePublicationIsRecoverableAndConvergent(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, firstTree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)
	if _, err := store.Publish(ctx, selector, capsule, firstTree); err != nil {
		t.Fatal(err)
	}

	_, replayTree := managerStoreFixture(t)
	winner, err := store.Publish(ctx, selector, capsule, replayTree)
	if err != nil {
		t.Fatal(err)
	}
	if winner != capsule {
		t.Fatalf("winner = %#v, want %#v", winner, capsule)
	}
}

func TestManagerStoreRejectsDivergentClaimWinner(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	first, firstTree := managerStoreFixture(t)
	selector := NewManagerSelector(
		first.PackageManager,
		first.Architecture,
	)
	if _, err := store.Publish(ctx, selector, first, firstTree); err != nil {
		t.Fatal(err)
	}

	second, secondTree := managerStoreFixture(t)
	second.Source.Digest = "sha256:" + strings.Repeat("3", 64)
	if _, err := store.Publish(
		ctx,
		selector,
		second,
		secondTree,
	); !errors.Is(err, ErrManagerAuthorityDiverged) {
		t.Fatalf("error = %v, want ErrManagerAuthorityDiverged", err)
	}
}

func TestManagerStoreVerifiesCandidateBeforeClaiming(t *testing.T) {
	for _, corrupt := range []string{"capsule", "tree"} {
		t.Run(corrupt, func(t *testing.T) {
			ctx := context.Background()
			documents := newMemoryManagerDocuments()
			trees := newTestManagerTrees(t)
			store, err := newManagerStore(documents, trees)
			if err != nil {
				t.Fatal(err)
			}
			capsule, tree := managerStoreFixture(t)
			selector := NewManagerSelector(
				capsule.PackageManager,
				capsule.Architecture,
			)
			capsuleDigest, err := ManagerCapsuleDigest(capsule)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt == "capsule" {
				capsuleKey, err := ManagerCapsuleKey(capsuleDigest)
				if err != nil {
					t.Fatal(err)
				}
				documents.objects[capsuleKey] = memoryManagerDocument{
					descriptor: managerDocument{
						Key:       capsuleKey,
						MediaType: ManagerCapsuleMediaType,
						SizeBytes: 2,
					},
					body: []byte("{}"),
				}
			} else {
				trees.corruptDigest = capsule.Tree.Digest
			}

			if _, err := store.Publish(
				ctx,
				selector,
				capsule,
				tree,
			); !errors.Is(err, ErrManagerAuthorityInvalid) {
				t.Fatalf("error = %v, want ErrManagerAuthorityInvalid", err)
			}
			claimKey, err := ManagerSelectorKey(selector)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := documents.objects[claimKey]; ok {
				t.Fatal("Publish claimed unverified candidate bytes")
			}
		})
	}
}

func TestManagerStoreVerifiesExistingClaimTreeBytes(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, tree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)
	if _, err := store.Publish(ctx, selector, capsule, tree); err != nil {
		t.Fatal(err)
	}
	trees.corruptDigest = capsule.Tree.Digest

	_, replayTree := managerStoreFixture(t)
	if _, err := store.Publish(
		ctx,
		selector,
		capsule,
		replayTree,
	); !errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf("error = %v, want ErrManagerAuthorityInvalid", err)
	}
}

func TestManagerStoreAuthenticatesDivergentWinnerBeforeClassifying(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	first, firstTree := managerStoreFixture(t)
	selector := NewManagerSelector(
		first.PackageManager,
		first.Architecture,
	)
	if _, err := store.Publish(ctx, selector, first, firstTree); err != nil {
		t.Fatal(err)
	}
	trees.corruptDigest = first.Tree.Digest
	second, secondTree := managerStoreFixture(t)
	second.Source.Digest = "sha256:" + strings.Repeat("3", 64)

	if _, err := store.Publish(
		ctx,
		selector,
		second,
		secondTree,
	); !errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf("error = %v, want ErrManagerAuthorityInvalid", err)
	}
}

func TestManagerStorePreservesBackendFailures(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, _ := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)
	claimKey, err := ManagerSelectorKey(selector)
	if err != nil {
		t.Fatal(err)
	}
	documents.readErrors[claimKey] = context.DeadlineExceeded

	_, err = store.Resolve(ctx, selector)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline cause", err)
	}
	if errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf("backend error was classified as invalid authority: %v", err)
	}
}

func TestManagerStorePreservesFinalTreeBackendFailure(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	trees.getErrors[2] = context.DeadlineExceeded
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, tree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)

	_, err = store.Publish(ctx, selector, capsule, tree)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline cause", err)
	}
	if errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf("backend error was classified as invalid authority: %v", err)
	}
}

func TestManagerStoreDistinguishesUnclaimedFromBrokenAuthority(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, tree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)
	if _, err := store.Resolve(
		ctx,
		selector,
	); !errors.Is(err, ErrManagerNotClaimed) {
		t.Fatalf("error = %v, want ErrManagerNotClaimed", err)
	}
	if _, err := store.Publish(ctx, selector, capsule, tree); err != nil {
		t.Fatal(err)
	}

	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	capsuleKey, err := ManagerCapsuleKey(capsuleDigest)
	if err != nil {
		t.Fatal(err)
	}
	delete(documents.objects, capsuleKey)
	if _, err := store.Resolve(
		ctx,
		selector,
	); !errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf("error = %v, want ErrManagerAuthorityInvalid", err)
	}
	_, replayTree := managerStoreFixture(t)
	if _, err := store.Publish(
		ctx,
		selector,
		capsule,
		replayTree,
	); !errors.Is(err, ErrManagerAuthorityInvalid) {
		t.Fatalf(
			"repair error = %v, want ErrManagerAuthorityInvalid",
			err,
		)
	}
	if _, ok := documents.objects[capsuleKey]; ok {
		t.Fatal("Publish repaired bytes behind an existing claim")
	}
}

func TestManagerStoreRecoversAfterUnclaimedObjects(t *testing.T) {
	ctx := context.Background()
	documents := newMemoryManagerDocuments()
	trees := newTestManagerTrees(t)
	store, err := newManagerStore(documents, trees)
	if err != nil {
		t.Fatal(err)
	}
	capsule, tree := managerStoreFixture(t)
	selector := NewManagerSelector(
		capsule.PackageManager,
		capsule.Architecture,
	)

	if _, err := trees.Publish(ctx, cas.Descriptor{
		Digest:    capsule.Tree.Digest,
		SizeBytes: capsule.Tree.SizeBytes,
		MediaType: capsule.Tree.MediaType,
	}, tree); err != nil {
		t.Fatal(err)
	}
	capsuleRaw, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		t.Fatal(err)
	}
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	capsuleKey, err := ManagerCapsuleKey(capsuleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := documents.Create(ctx, managerDocument{
		Key:       capsuleKey,
		MediaType: ManagerCapsuleMediaType,
		SizeBytes: int64(len(capsuleRaw)),
	}, capsuleRaw); err != nil {
		t.Fatal(err)
	}

	_, replayTree := managerStoreFixture(t)
	winner, err := store.Publish(ctx, selector, capsule, replayTree)
	if err != nil {
		t.Fatal(err)
	}
	if winner != capsule {
		t.Fatalf("winner = %#v, want %#v", winner, capsule)
	}
}

type memoryManagerDocuments struct {
	objects    map[string]memoryManagerDocument
	readErrors map[string]error
	order      *[]string
}

type memoryManagerDocument struct {
	descriptor managerDocument
	body       []byte
}

func newMemoryManagerDocuments() *memoryManagerDocuments {
	order := make([]string, 0)
	return &memoryManagerDocuments{
		objects:    make(map[string]memoryManagerDocument),
		readErrors: make(map[string]error),
		order:      &order,
	}
}

func (s *memoryManagerDocuments) Create(
	_ context.Context,
	descriptor managerDocument,
	body []byte,
) (bool, error) {
	if _, ok := s.objects[descriptor.Key]; ok {
		return false, nil
	}
	s.objects[descriptor.Key] = memoryManagerDocument{
		descriptor: descriptor,
		body:       append([]byte(nil), body...),
	}
	*s.order = append(*s.order, "document:"+descriptor.Key)
	return true, nil
}

func (s *memoryManagerDocuments) Read(
	_ context.Context,
	key string,
) (managerDocument, io.ReadCloser, error) {
	if err := s.readErrors[key]; err != nil {
		return managerDocument{}, nil, err
	}
	object, ok := s.objects[key]
	if !ok {
		return managerDocument{}, nil, errManagerObjectNotFound
	}
	return object.descriptor, io.NopCloser(bytes.NewReader(object.body)), nil
}

type testManagerTrees struct {
	store         *cas.File
	order         *[]string
	corruptDigest string
	getCalls      int
	getErrors     map[int]error
}

func newTestManagerTrees(t *testing.T) *testManagerTrees {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	order := make([]string, 0)
	return &testManagerTrees{
		store:     store,
		order:     &order,
		getErrors: make(map[int]error),
	}
}

func (s *testManagerTrees) Publish(
	ctx context.Context,
	expected cas.Descriptor,
	file *os.File,
) (cas.Object, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return cas.Object{}, err
	}
	object, err := s.store.Put(
		ctx,
		expected.MediaType,
		io.LimitReader(file, expected.SizeBytes+1),
	)
	if err != nil {
		return cas.Object{}, err
	}
	if object.Digest != expected.Digest ||
		object.SizeBytes != expected.SizeBytes ||
		object.MediaType != expected.MediaType {
		return cas.Object{}, errors.New("tree does not match its descriptor")
	}
	*s.order = append(*s.order, "tree:"+expected.Digest)
	return object, nil
}

func (s *testManagerTrees) Stat(
	ctx context.Context,
	digest string,
) (cas.Object, error) {
	return s.store.Stat(ctx, digest)
}

func (s *testManagerTrees) Get(
	ctx context.Context,
	digest string,
) (io.ReadCloser, error) {
	s.getCalls++
	if err := s.getErrors[s.getCalls]; err != nil {
		return nil, err
	}
	if digest == s.corruptDigest {
		return &managerDigestMismatchReader{
			Reader: bytes.NewReader([]byte("manager tree")),
		}, nil
	}
	return s.store.Get(ctx, digest)
}

type managerDigestMismatchReader struct {
	*bytes.Reader
}

func (r *managerDigestMismatchReader) Read(body []byte) (int, error) {
	read, err := r.Reader.Read(body)
	if errors.Is(err, io.EOF) {
		return read, cas.ErrDigestMismatch
	}
	return read, err
}

func (*managerDigestMismatchReader) Close() error {
	return nil
}

func managerStoreFixture(t *testing.T) (ManagerCapsule, *os.File) {
	t.Helper()
	body := []byte("manager tree")
	file, err := os.CreateTemp(t.TempDir(), "manager-tree-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = file.Close()
	})
	capsule := managerCapsuleFixture(
		PackageManagerBun,
		ArchitectureAArch64,
	)
	capsule.Tree.Digest = digestBytes(body)
	capsule.Tree.SizeBytes = int64(len(body))
	return capsule, file
}

var _ managerDocuments = (*memoryManagerDocuments)(nil)
var _ cas.ImmutableStore = (*testManagerTrees)(nil)
