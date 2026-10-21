//go:build linux

package computerhost

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type gatedProgramStore struct {
	cas.Store
	entered      chan string
	release      chan struct{}
	failedDigest string
	failure      error
	canceled     chan struct{}
	finish       chan struct{}
}

func (s *gatedProgramStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	s.entered <- digest
	<-s.release
	if s.failedDigest != "" {
		if digest == s.failedDigest {
			return nil, s.failure
		}
		<-ctx.Done()
		close(s.canceled)
		<-s.finish
		return nil, ctx.Err()
	}
	return s.Store.Get(ctx, digest)
}

func TestProgramStagingOverlapsReadsAndJoinsCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "peer cancellation"}[fail], func(t *testing.T) {
			store := &fakeCAS{objects: map[string][]byte{}}
			r := store.put(artifact.RuntimeArtifactMediaType, []byte("runtime"))
			p := store.put(artifact.ProgramArtifactMediaType, []byte("program"))
			index := artifacttest.ProgramMetadata(t)
			index.RuntimeDigest = r.Digest
			canonical, err := artifact.CanonicalProgramMetadata(index)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := artifact.RuntimeDescriptor{Digest: r.Digest, SizeBytes: r.SizeBytes, MediaType: r.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion}
			gate := &gatedProgramStore{Store: store, entered: make(chan string, 2), release: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{})}
			if fail {
				gate.failedDigest = p.Digest
				gate.failure = errors.New("program read failed")
			}
			machines := &PreparedMachines{CAS: gate, PlatformStore: gate, RuntimeArchitecture: definition.ArchitectureX8664, verifiedRuntimes: map[artifact.RuntimeDescriptor]artifact.RuntimeIndex{descriptor: {Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract}}, programDescriptor: artifact.ProgramDescriptor{Digest: p.Digest, SizeBytes: p.SizeBytes, MediaType: p.MediaType}, programMetadata: &index}
			program := &workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: r.Digest, SizeBytes: r.SizeBytes, MediaType: r.MediaType}, Artifact: workerapi.CASObject{Digest: p.Digest, SizeBytes: p.SizeBytes, MediaType: p.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// Release every test gate before joining, including on assertion failure.
			released, finished := false, false
			done := make(chan error, 1)
			finishedWork := make(chan struct{})
			defer func() {
				cancel()
				if !released {
					close(gate.release)
				}
				if !finished {
					close(gate.finish)
				}
				<-finishedWork
			}()
			directory := t.TempDir()
			go func() {
				defer close(finishedWork)
				images, err := machines.prepareProgramArtifacts(ctx, directory, "parallel-staging", program)
				if images != nil {
					err = errors.Join(err, images.close())
				}
				done <- err
			}()
			seen := map[string]bool{}
			for range 2 {
				select {
				case digest := <-gate.entered:
					seen[digest] = true
				case <-ctx.Done():
					t.Fatal("artifact reads did not overlap")
				}
			}
			if !seen[r.Digest] || !seen[p.Digest] {
				t.Fatal("did not start both descriptors")
			}
			close(gate.release)
			released = true
			if fail {
				select {
				case <-gate.canceled:
				case <-ctx.Done():
					t.Fatal("peer did not observe cancellation")
				}
				select {
				case err := <-done:
					t.Fatalf("returned before peer joined: %v", err)
				default:
				}
			}
			close(gate.finish)
			finished = true
			select {
			case err := <-done:
				if fail && !errors.Is(err, gate.failure) {
					t.Fatalf("lost source failure: %v", err)
				}
				if !fail && err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("staging did not join")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging leaked files: %v %v", entries, err)
			}
		})
	}
}
