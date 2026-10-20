//go:build linux

package computerhost

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/objectcache"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPreparedObjectCacheOwnsCopiesAcrossConsumers(t *testing.T) {
	store := &fakeCAS{objects: map[string][]byte{}}
	runtimeObject := store.put(artifact.RuntimeArtifactMediaType, []byte("runtime"))
	programObject := store.put(artifact.ProgramArtifactMediaType, []byte("program"))
	index := artifacttest.ProgramMetadata(t)
	index.RuntimeDigest = runtimeObject.Digest
	canonical, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	rd := artifact.RuntimeDescriptor{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion}
	pd := artifact.ProgramDescriptor{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}
	cache, err := objectcache.Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	p := &PreparedMachines{CAS: store, PlatformStore: store, ObjectCache: cache, RuntimeArchitecture: definition.ArchitectureX8664, verifiedRuntimes: map[artifact.RuntimeDescriptor]artifact.RuntimeIndex{rd: {Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract}}, programDescriptor: pd, programMetadata: &index}
	program := &workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: rd.Digest, SizeBytes: rd.SizeBytes, MediaType: rd.MediaType}, Artifact: workerapi.CASObject{Digest: pd.Digest, SizeBytes: pd.SizeBytes, MediaType: pd.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}
	firstDirectory := t.TempDir()
	first, err := p.prepareProgramArtifacts(t.Context(), firstDirectory, "first", program)
	if err != nil {
		t.Fatal(err)
	}

	boot, err := bootProgram(program)
	if err != nil {
		t.Fatal(err)
	}
	beforeProgram, beforeRuntime := store.getCalls[pd.Digest], store.getCalls[rd.Digest]
	savedRuntimeMemo := p.verifiedRuntimes
	p.verifiedRuntimes = nil
	mounted, err := p.prepareProgramArtifactsWithBoot(t.Context(), t.TempDir(), "boot-mounted", program, boot)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.verifiedRuntimes) != 0 {
		t.Fatal("boot drive reuse created a host verification receipt")
	}
	p.verifiedRuntimes = savedRuntimeMemo
	if !mounted.bootArtifact || !mounted.bootRuntime || mounted.runtime != nil || mounted.artifact != nil || store.getCalls[pd.Digest] != beforeProgram || store.getCalls[rd.Digest] != beforeRuntime {
		t.Fatal("matching boot drives were staged again")
	}
	if err = mounted.close(); err != nil {
		t.Fatal(err)
	}

	// A later Program can reuse its verified metadata without restaging either
	// image; its Runtime remains supplied by the boot mount.
	otherBoot := *boot
	otherBoot.Artifact.Digest = sha256sum.DigestBytes([]byte("other boot program"))
	reused, err := p.prepareSessionProgramArtifactsWithBoot(t.Context(), t.TempDir(), "later-session", program, &otherBoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reused.bootRuntime || reused.bootArtifact || reused.runtime != nil || reused.artifact != nil || store.getCalls[pd.Digest] != beforeProgram || store.getCalls[rd.Digest] != beforeRuntime {
		t.Fatal("later cached Session Program was restaged")
	}
	if err := reused.close(); err != nil {
		t.Fatal(err)
	}
	p.seedProgramArtifacts(t.Context(), first, cas.Descriptor(pd))
	if cache.Snapshot().Seeds != 2 {
		t.Fatal("artifacts not cached")
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(firstDirectory); err != nil {
		t.Fatal(err)
	}
	delete(store.objects, rd.Digest)
	delete(store.objects, pd.Digest)
	// A second consumer requires its own real sealed snapshots even though the
	// original staging directory and remote bytes are gone.
	second, err := p.prepareProgramArtifacts(t.Context(), t.TempDir(), "second", program)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if second.runtime == nil || second.artifact == nil {
		t.Fatal("cache bypassed consumer snapshots")
	}
	if store.getCalls[pd.Digest] != 1 || store.getCalls[rd.Digest] != 1 {
		t.Fatal("cache hit fetched remote artifacts")
	}
	session, err := p.prepareSessionProgramArtifacts(t.Context(), t.TempDir(), "session", program)
	if err != nil {
		t.Fatal(err)
	}
	if session.runtimeRead == nil || session.programRead == nil || session.runtime != nil || session.artifact != nil {
		t.Fatal("Session did not borrow exact cached pair")
	}
	if err := session.close(); err != nil {
		t.Fatal(err)
	}
	// Missing second object must release the first lease, including on shutdown.
	partial, err := objectcache.Open(filepath.Join(t.TempDir(), "partial"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := partial.Seed(t.Context(), "runtime", cas.Descriptor{Digest: rd.Digest, SizeBytes: rd.SizeBytes, MediaType: rd.MediaType}, bytes.NewBufferString("runtime")); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	p.ObjectCache = partial
	session, err = p.prepareSessionProgramArtifacts(t.Context(), t.TempDir(), "partial", program)
	if err != nil {
		t.Fatal(err)
	}
	if session.runtimeRead != nil || session.programRead != nil {
		t.Fatal("partial transfer retained lease")
	}
	session.close()
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	p.ObjectCache = cache
	program.IndexDigest = sha256sum.DigestBytes([]byte("other"))
	before := cache.Snapshot()
	if images, err := p.prepareSessionProgramArtifacts(t.Context(), t.TempDir(), "wrong-session", program); err == nil {
		images.close()
		t.Fatal("Session cache bypassed target authority")
	}
	if after := cache.Snapshot(); after.Hits != before.Hits {
		t.Fatal("leased bytes before checking metadata authority")
	}

	if images, err := p.prepareProgramArtifacts(t.Context(), t.TempDir(), "wrong-target", program); err == nil {
		images.close()
		t.Fatal("cache bypassed target authority")
	}
}
