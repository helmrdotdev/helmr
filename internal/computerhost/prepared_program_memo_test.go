package computerhost

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

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPreparedProgramMemoDescriptorAndOwnership(t *testing.T) {
	machines := &PreparedMachines{}
	descriptor := artifact.ProgramDescriptor{Digest: "sha256:one", SizeBytes: 42, MediaType: artifact.ProgramArtifactMediaType}
	limit := int64(3)
	index := artifact.ProgramIndex{Queues: []definition.QueueInput{{Name: "queue", ConcurrencyLimit: &limit}}, Declarations: []artifact.ProgramIndexDeclaration{{Locator: &artifact.ProgramLocator{ExportName: "original"}}}}
	calls := 0
	verify := func() (artifact.ProgramIndex, error) { calls++; return index, nil }
	first, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil {
		t.Fatal(err)
	}
	first.Declarations[0].Locator.ExportName = "changed"
	*index.Queues[0].ConcurrencyLimit = 9
	hit, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || calls != 1 || hit.Declarations[0].Locator.ExportName != "original" || *hit.Queues[0].ConcurrencyLimit != 3 {
		t.Fatalf("hit = %+v, calls %d, error %v", hit, calls, err)
	}
	hit.Declarations[0].Locator.ExportName = "changed hit"
	*hit.Queues[0].ConcurrencyLimit = 10
	again, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || !reflect.DeepEqual(again.Declarations[0].Locator, &artifact.ProgramLocator{ExportName: "original"}) || *again.Queues[0].ConcurrencyLimit != 3 {
		t.Fatal("returned hit aliases memo", err)
	}
	// Reflect over every actual descriptor field so future key fields need coverage.
	for i := 0; i < reflect.TypeFor[artifact.ProgramDescriptor]().NumField(); i++ {
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
		if _, err := machines.verifyProgram(t.Context(), changed, verify); err != nil || calls != before+1 {
			t.Fatalf("field %d did not invalidate: %v", i, err)
		}
		if _, err := machines.verifyProgram(t.Context(), descriptor, verify); err != nil || calls != before+2 {
			t.Fatalf("field %d did not evict: %v", i, err)
		}
	}
}

func TestPreparedProgramMemoPublishesOnlySuccess(t *testing.T) {
	machines := &PreparedMachines{}
	descriptor := artifact.ProgramDescriptor{Digest: "original"}
	good := artifact.ProgramIndex{RuntimeContract: "original"}
	if _, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramIndex, error) { return good, nil }); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{errors.New("verification failed"), context.Canceled, context.DeadlineExceeded} {
		for range 2 {
			_, err := machines.verifyProgram(t.Context(), artifact.ProgramDescriptor{Digest: "failed"}, func() (artifact.ProgramIndex, error) { return good, failure })
			if !errors.Is(err, failure) {
				t.Fatalf("failure cached or lost: %v", err)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := machines.verifyProgram(ctx, artifact.ProgramDescriptor{Digest: "cancel-after-success"}, func() (artifact.ProgramIndex, error) { cancel(); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = machines.verifyProgram(ctx, descriptor, func() (artifact.ProgramIndex, error) { t.Fatal("canceled hit verified"); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramIndex, error) { t.Fatal("failed miss replaced success"); return good, nil })
	if err != nil || got.RuntimeContract != "original" {
		t.Fatal(got, err)
	}
	calls := 0
	for range 2 {
		_, err = machines.verifyProgram(t.Context(), artifact.ProgramDescriptor{Digest: "cancel-after-success"}, func() (artifact.ProgramIndex, error) { calls++; return good, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("successful publication calls %d", calls)
	}
}

func TestPreparedProgramMemoConcurrentMisses(t *testing.T) {
	machines := &PreparedMachines{}
	const n = 8
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	descriptor := artifact.ProgramDescriptor{Digest: "same"}
	for range n {
		wg.Go(func() {
			got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramIndex, error) {
				entered <- struct{}{}
				<-release
				return artifact.ProgramIndex{Queues: []definition.QueueInput{{Name: "original"}}}, nil
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
			t.Fatal("misses serialized under machines lock")
		}
	}
	close(release)
	wg.Wait()
	for range n {
		wg.Go(func() {
			got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramIndex, error) {
				t.Error("missing published result")
				return artifact.ProgramIndex{}, nil
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
	index := artifact.ProgramIndex{
		Architecture:       definition.ArchitectureX8664,
		ConfigResultDigest: digest,
		Declarations: []artifact.ProgramIndexDeclaration{{
			Kind:       definition.KindTask,
			DeclaredID: "task",
			Task: &definition.TaskManifest{
				Payload: definition.SchemaManifest{Kind: definition.SchemaKindNone},
				Run: definition.RunManifest{
					Queue:         "task/task",
					MaxDurationMs: 900000,
					Retry:         definition.RetryManifest{Enabled: false},
				},
			},
			Locator: &artifact.ProgramLocator{
				ExportName: "task",
				ModulePath: "helmr/app/entry-0.mjs",
				Slot:       artifact.DeclarationSlotHandler,
			},
		}},
		Queues: []definition.QueueInput{{
			Name: "task/task",
		}},
		RuntimeContract: definition.RuntimeContract,
		RuntimeDigest:   "sha256:" + strings.Repeat("f", 64),
	}

	store := &fakeCAS{objects: map[string][]byte{}}
	runtimeObject := store.put(artifact.RuntimeArtifactMediaType, []byte("runtime"))
	programObject := store.put(artifact.ProgramArtifactMediaType, []byte("program"))
	index.RuntimeDigest = runtimeObject.Digest
	canonical, err := artifact.CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor := artifact.RuntimeDescriptor{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion}
	descriptor := artifact.ProgramDescriptor{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}
	machines := &PreparedMachines{CAS: store, PlatformStore: store, RuntimeArchitecture: definition.ArchitectureX8664, verifiedRuntimes: map[artifact.RuntimeDescriptor]artifact.RuntimeIndex{runtimeDescriptor: {Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract}}}
	// Inject only the isolated verifier result; exercise real snapshots and the
	// production prepareProgram authority path on every hit.
	if _, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramIndex, error) { return index, nil }); err != nil {
		t.Fatal(err)
	}
	target := workerapi.InstanceReconcileTarget{ID: "memo-test", Source: workerapi.InstanceSource{ComputerArchitecture: string(definition.ArchitectureX8664), Program: &workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType}, Artifact: workerapi.CASObject{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}}}
	run := func(want string) {
		t.Helper()
		dir := t.TempDir()
		drives, cleanup, err := machines.prepareProgram(t.Context(), dir, target)
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
	// Both artifacts have valid independent verification receipts, but a Program
	// may only launch with the exact Runtime selected when it was built.
	otherRuntime := store.put(artifact.RuntimeArtifactMediaType, []byte("another runtime"))
	otherDescriptor := runtimeDescriptor
	otherDescriptor.Digest = otherRuntime.Digest
	otherDescriptor.SizeBytes = otherRuntime.SizeBytes
	machines.verifiedRuntimes[otherDescriptor] = machines.verifiedRuntimes[runtimeDescriptor]
	target.Source.Program.Runtime = workerapi.CASObject{Digest: otherRuntime.Digest, SizeBytes: otherRuntime.SizeBytes, MediaType: otherRuntime.MediaType}
	run("runtime reservation authority")
	target.Source.Program.Runtime = workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType}
	target.Source.Program.IndexDigest = "sha256:" + strings.Repeat("0", 64)
	run("deployment authority")
	target.Source.Program.IndexDigest = sha256sum.DigestBytes(canonical)
	target.Source.Program.DeploymentID = ""
	run("deployment id")
	target.Source.Program.DeploymentID = "deployment"
	target.Source.ComputerArchitecture = "aarch64"
	run("computer architecture")
	target.Source.ComputerArchitecture = string(definition.ArchitectureX8664)
	for _, bad := range []artifact.ProgramIndex{{Architecture: definition.ArchitectureX8664, RuntimeContract: "wrong"}, {Architecture: definition.RuntimeArchitecture("aarch64"), RuntimeContract: definition.RuntimeContract}} {
		machines.mu.Lock()
		machines.programIndex = &bad
		machines.mu.Unlock()
		run("runtime reservation authority")
	}
	owned := index.Clone()
	machines.programIndex = &owned
	store.objects[programObject.Digest] = []byte("tamper!")
	run("digest")
}
