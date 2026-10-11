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
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPreparedProgramMemoDescriptorAndOwnership(t *testing.T) {
	machines := &PreparedMachines{}
	descriptor := artifact.ProgramDescriptor{Digest: "sha256:one", SizeBytes: 42, MediaType: artifact.ProgramArtifactMediaType}
	index := artifacttest.ProgramMetadata(t)
	index.Definitions[0].Agent.Triggers["nightly"] = definition.CronTrigger{Input: []byte(`[{"type":"text","text":"{\"original\":true}"}]`)}
	index.Definitions[2].Computer.Secrets = []definition.SecretBinding{{SecretID: "original", Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	index.Definitions[2].Computer.Seed.Config.Env = []string{"KEY=original"}
	original := index.Clone()
	mutate := func(value artifact.ProgramMetadata) {
		value.Definitions[0].Agent.Triggers["nightly"].Input[2] = 'X'
		value.Definitions[2].Computer.Secrets = []definition.SecretBinding{{SecretID: "changed", Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
		value.Definitions[2].Computer.Seed.Config.Env[0] = "KEY=changed"
	}
	calls := 0
	verify := func() (artifact.ProgramMetadata, error) { calls++; return index, nil }
	first, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil {
		t.Fatal(err)
	}
	mutate(first)
	mutate(index)
	hit, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || calls != 1 || !reflect.DeepEqual(hit, original) {
		t.Fatalf("hit = %+v, calls %d, error %v", hit, calls, err)
	}
	mutate(hit)
	again, err := machines.verifyProgram(t.Context(), descriptor, verify)
	if err != nil || !reflect.DeepEqual(again, original) {
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
	good := artifact.ProgramMetadata{RuntimeContract: "original"}
	if _, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) { return good, nil }); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{errors.New("verification failed"), context.Canceled, context.DeadlineExceeded} {
		for range 2 {
			_, err := machines.verifyProgram(t.Context(), artifact.ProgramDescriptor{Digest: "failed"}, func() (artifact.ProgramMetadata, error) { return good, failure })
			if !errors.Is(err, failure) {
				t.Fatalf("failure cached or lost: %v", err)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := machines.verifyProgram(ctx, artifact.ProgramDescriptor{Digest: "cancel-after-success"}, func() (artifact.ProgramMetadata, error) { cancel(); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = machines.verifyProgram(ctx, descriptor, func() (artifact.ProgramMetadata, error) { t.Fatal("canceled hit verified"); return good, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) { t.Fatal("failed miss replaced success"); return good, nil })
	if err != nil || got.RuntimeContract != "original" {
		t.Fatal(got, err)
	}
	calls := 0
	for range 2 {
		_, err = machines.verifyProgram(t.Context(), artifact.ProgramDescriptor{Digest: "cancel-after-success"}, func() (artifact.ProgramMetadata, error) { calls++; return good, nil })
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
			got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) {
				entered <- struct{}{}
				<-release
				return artifact.ProgramMetadata{Definitions: []artifact.ProgramDefinition{{DeclaredID: "original"}}}, nil
			})
			if err != nil {
				t.Error(err)
				return
			}
			got.Definitions[0].DeclaredID = "caller mutation"
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
			got, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) {
				t.Error("missing published result")
				return artifact.ProgramMetadata{}, nil
			})
			if err != nil || len(got.Definitions) != 1 || got.Definitions[0].DeclaredID != "original" {
				t.Error("corrupted memo", err)
				return
			}
			got.Definitions[0].DeclaredID = "mutated hit"
		})
	}
	wg.Wait()
}

func TestPreparedProgramMemoHitPreservesSnapshotAndTargetAuthority(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("artifact snapshots require Linux")
	}
	index := artifacttest.ProgramMetadata(t)

	store := &fakeCAS{objects: map[string][]byte{}}
	runtimeObject := store.put(artifact.RuntimeArtifactMediaType, []byte("runtime"))
	programObject := store.put(artifact.ProgramArtifactMediaType, []byte("program"))
	index.RuntimeDigest = runtimeObject.Digest
	canonical, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor := artifact.RuntimeDescriptor{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion}
	descriptor := artifact.ProgramDescriptor{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}
	machines := &PreparedMachines{CAS: store, PlatformStore: store, RuntimeArchitecture: definition.ArchitectureX8664, verifiedRuntimes: map[artifact.RuntimeDescriptor]artifact.RuntimeIndex{runtimeDescriptor: {Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract}}}
	// Inject only the isolated verifier result; exercise real snapshots and the
	// production artifact preparation authority path on every hit.
	if _, err := machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) { return index, nil }); err != nil {
		t.Fatal(err)
	}
	program := &workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType}, Artifact: workerapi.CASObject{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}
	run := func(want string) {
		t.Helper()
		dir := t.TempDir()
		images, err := machines.prepareProgramArtifacts(t.Context(), dir, "memo-test", program)
		if images != nil {
			if closeErr := images.close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if want == "" {
			if err != nil || images == nil || images.runtime == nil || images.artifact == nil {
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
	program.Runtime = workerapi.CASObject{Digest: otherRuntime.Digest, SizeBytes: otherRuntime.SizeBytes, MediaType: otherRuntime.MediaType}
	run("runtime reservation authority")
	program.Runtime = workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType}
	program.IndexDigest = "sha256:" + strings.Repeat("0", 64)
	run("deployment authority")
	program.IndexDigest = sha256sum.DigestBytes(canonical)
	program.DeploymentID = ""
	run("deployment id")
	program.DeploymentID = "deployment"
	for _, bad := range []artifact.ProgramMetadata{{Architecture: definition.ArchitectureX8664, RuntimeContract: "wrong"}, {Architecture: definition.RuntimeArchitecture("aarch64"), RuntimeContract: definition.RuntimeContract}} {
		machines.mu.Lock()
		machines.programMetadata = &bad
		machines.mu.Unlock()
		run("runtime reservation authority")
	}
	owned := index.Clone()
	machines.programMetadata = &owned
	store.objects[programObject.Digest] = []byte("tamper!")
	run("digest")
}
