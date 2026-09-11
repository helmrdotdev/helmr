package executor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPreparedProgramMemoDescriptorAndOwnership(t *testing.T) {
	pool := &PreparedRuntimePool{}
	descriptor := deployment.ProgramDescriptor{Digest: "sha256:one", SizeBytes: 42, MediaType: deployment.ProgramArtifactMediaType}
	limit := int64(3)
	index := deployment.ProgramIndex{Queues: []deployment.QueueInput{{Name: "queue", ConcurrencyLimit: &limit}}, Declarations: []deployment.ProgramIndexDeclaration{{Locator: &deployment.ProgramLocator{ExportName: "original"}}}}
	calls := 0
	verify := func() (deployment.ProgramIndex, error) { calls++; return index, nil }
	first, err := pool.verifyProgram(t.Context(), descriptor, verify)
	if err != nil {
		t.Fatal(err)
	}
	first.Declarations[0].Locator.ExportName = "changed"
	*index.Queues[0].ConcurrencyLimit = 9
	hit, err := pool.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || calls != 1 || hit.Declarations[0].Locator.ExportName != "original" || *hit.Queues[0].ConcurrencyLimit != 3 {
		t.Fatalf("hit = %+v, calls %d, error %v", hit, calls, err)
	}
	hit.Declarations[0].Locator.ExportName = "changed hit"
	*hit.Queues[0].ConcurrencyLimit = 10
	again, err := pool.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || !reflect.DeepEqual(again.Declarations[0].Locator, &deployment.ProgramLocator{ExportName: "original"}) || *again.Queues[0].ConcurrencyLimit != 3 {
		t.Fatal("returned hit aliases memo", err)
	}
	// Reflect over every actual descriptor field so future key fields need coverage.
	for i := 0; i < reflect.TypeFor[deployment.ProgramDescriptor]().NumField(); i++ {
		changed := descriptor
		field := reflect.ValueOf(&changed).Elem().Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(field.String() + "x")
		case reflect.Int64:
			field.SetInt(field.Int() + 1)
		default:
			t.Fatal("add descriptor field mutation")
		}
		before := calls
		if _, err := pool.verifyProgram(t.Context(), changed, verify); err != nil || calls != before+1 {
			t.Fatalf("field %d did not invalidate: %v", i, err)
		}
		if _, err := pool.verifyProgram(t.Context(), descriptor, verify); err != nil || calls != before+2 {
			t.Fatalf("field %d did not evict: %v", i, err)
		}
	}
}

func TestPreparedProgramMemoPublishesOnlySuccess(t *testing.T) {
	pool := &PreparedRuntimePool{}
	descriptor := deployment.ProgramDescriptor{Digest: "original"}
	good := deployment.ProgramIndex{RuntimeContract: "original"}
	if _, err := pool.verifyProgram(t.Context(), descriptor, func() (deployment.ProgramIndex, error) { return good, nil }); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{errors.New("verification failed"), context.Canceled, context.DeadlineExceeded} {
		for range 2 {
			_, err := pool.verifyProgram(t.Context(), deployment.ProgramDescriptor{Digest: "failed"}, func() (deployment.ProgramIndex, error) { return good, failure })
			if !errors.Is(err, failure) {
				t.Fatalf("failure cached or lost: %v", err)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := pool.verifyProgram(ctx, deployment.ProgramDescriptor{Digest: "cancel-after-success"}, func() (deployment.ProgramIndex, error) { cancel(); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = pool.verifyProgram(ctx, descriptor, func() (deployment.ProgramIndex, error) { t.Fatal("canceled hit verified"); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, err := pool.verifyProgram(t.Context(), descriptor, func() (deployment.ProgramIndex, error) { t.Fatal("failed miss replaced success"); return good, nil })
	if err != nil || got.RuntimeContract != "original" {
		t.Fatal(got, err)
	}
	calls := 0
	for range 2 {
		_, err = pool.verifyProgram(t.Context(), deployment.ProgramDescriptor{Digest: "cancel-after-success"}, func() (deployment.ProgramIndex, error) { calls++; return good, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("successful publication calls %d", calls)
	}
}

func TestPreparedProgramMemoConcurrentMisses(t *testing.T) {
	pool := &PreparedRuntimePool{}
	const n = 8
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	descriptor := deployment.ProgramDescriptor{Digest: "same"}
	for range n {
		wg.Go(func() {
			got, err := pool.verifyProgram(t.Context(), descriptor, func() (deployment.ProgramIndex, error) {
				entered <- struct{}{}
				<-release
				return deployment.ProgramIndex{Queues: []deployment.QueueInput{{Name: "original"}}}, nil
			})
			if err != nil {
				t.Error(err)
				return
			}
			got.Queues[0].Name = "caller mutation"
		})
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range n {
		select {
		case <-entered:
		case <-timer.C:
			close(release)
			wg.Wait()
			t.Fatal("misses serialized under pool lock")
		}
	}
	close(release)
	wg.Wait()
	for range n {
		wg.Go(func() {
			got, err := pool.verifyProgram(t.Context(), descriptor, func() (deployment.ProgramIndex, error) {
				t.Error("missing published result")
				return deployment.ProgramIndex{}, nil
			})
			if err != nil || len(got.Queues) != 1 || got.Queues[0].Name != "original" {
				t.Error("corrupted memo", err)
				return
			}
			got.Queues[0].Name = "mutated hit"
		})
	}
	wg.Wait()
}

func TestPreparedProgramMemoHitPreservesSnapshotAndTargetAuthority(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("artifact snapshots require Linux")
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	index := deployment.ProgramIndex{
		Architecture:       deployment.ArchitectureX8664,
		ConfigResultDigest: digest,
		Declarations: []deployment.ProgramIndexDeclaration{{
			Kind:       deployment.DefinitionKindTask,
			DeclaredID: "task",
			Task: &deployment.TaskManifest{
				Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone},
				Run: deployment.RunManifest{
					Queue:         "task/task",
					MaxDurationMs: 900000,
					Retry:         deployment.RetryManifest{Enabled: false},
				},
			},
			Locator: &deployment.ProgramLocator{
				ExportName: "task",
				ModulePath: ".helmr/modules/" + strings.Repeat("1", 64) + ".mjs",
				Slot:       deployment.DeclarationSlotHandler,
			},
		}},
		Queues: []deployment.QueueInput{{
			Name: "task/task",
		}},
		RuntimeContract: deployment.RuntimeContract,
		RuntimeDigest:   "sha256:" + strings.Repeat("f", 64),
	}

	store := &fakeCAS{objects: map[string][]byte{}}
	runtimeObject := store.put(deployment.RuntimeArtifactMediaType, []byte("runtime"))
	programObject := store.put(deployment.ProgramArtifactMediaType, []byte("program"))
	index.RuntimeDigest = runtimeObject.Digest
	canonical, err := deployment.CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor := deployment.RuntimeDescriptor{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType, Architecture: deployment.ArchitectureX8664, RuntimeContract: deployment.RuntimeContract, FormatVersion: deployment.RuntimeDescriptorFormatVersion}
	descriptor := deployment.ProgramDescriptor{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}
	pool := &PreparedRuntimePool{CAS: store, PlatformStore: store, RuntimeArchitecture: deployment.ArchitectureX8664, verifiedRuntimes: map[deployment.RuntimeDescriptor]deployment.RuntimeIndex{runtimeDescriptor: {Architecture: deployment.ArchitectureX8664, RuntimeContract: deployment.RuntimeContract}}}
	// Inject only the isolated verifier result; exercise real snapshots and the
	// production prepareProgram authority path on every hit.
	if _, err := pool.verifyProgram(t.Context(), descriptor, func() (deployment.ProgramIndex, error) { return index, nil }); err != nil {
		t.Fatal(err)
	}
	target := workerapi.RuntimeReconcileTarget{ID: "memo-test", Source: workerapi.RuntimeSource{WorkspaceArchitecture: string(deployment.ArchitectureX8664), Program: &workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType}, Artifact: workerapi.CASObject{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}}}
	run := func(want string) {
		t.Helper()
		dir := t.TempDir()
		drives, cleanup, err := pool.prepareProgram(t.Context(), dir, target)
		if closeErr := cleanup(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if want == "" {
			if err != nil || len(drives) != 2 {
				t.Fatalf("prepare: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v, want %q", err, want)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("snapshot cleanup: %v, %v", entries, err)
		}
	}
	run("")
	run("")
	if store.getCalls[programObject.Digest] != 2 || store.getCalls[runtimeObject.Digest] != 2 {
		t.Fatal("hit bypassed snapshot reads")
	}
	target.Source.Program.IndexDigest = "sha256:" + strings.Repeat("0", 64)
	run("deployment authority")
	target.Source.Program.IndexDigest = sha256sum.DigestBytes(canonical)
	target.Source.Program.DeploymentID = ""
	run("deployment id")
	target.Source.Program.DeploymentID = "deployment"
	target.Source.WorkspaceArchitecture = "aarch64"
	run("workspace architecture")
	target.Source.WorkspaceArchitecture = string(deployment.ArchitectureX8664)
	for _, bad := range []deployment.ProgramIndex{{Architecture: deployment.ArchitectureX8664, RuntimeContract: "wrong"}, {Architecture: deployment.RuntimeArchitecture("aarch64"), RuntimeContract: deployment.RuntimeContract}} {
		pool.mu.Lock()
		pool.programIndex = &bad
		pool.mu.Unlock()
		run("runtime reservation authority")
	}
	owned := index.Clone()
	pool.programIndex = &owned
	store.objects[programObject.Digest] = []byte("tamper!")
	run("digest")
}
