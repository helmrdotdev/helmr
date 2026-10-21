package guestd

import (
	"context"
	"errors"
	"io"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"google.golang.org/protobuf/proto"
)

type bootProgramInput struct{ transfers int }

func (i *bootProgramInput) copyArtifact(context.Context, agentv1.SessionProgramRequest_Kind, *agentv1.SessionProgramArtifact, io.Writer) error {
	i.transfers++
	return errors.New("unexpected transfer")
}
func (*bootProgramInput) releaseStart(context.Context) (*agentv1.SessionGrant, error) {
	return nil, errors.New("start release must remain separate")
}
func testBootProgram() *agentv1.SessionProgram {
	return &agentv1.SessionProgram{Runtime: &agentv1.SessionProgramArtifact{Digest: sha256sum.DigestBytes([]byte("runtime")), SizeBytes: 4096}, Artifact: &agentv1.SessionProgramArtifact{Digest: sha256sum.DigestBytes([]byte("program")), SizeBytes: 8192}}
}
func TestBootProgramLeasesRetainPlatformMounts(t *testing.T) {
	program := testBootProgram()
	store := &agentProgramStore{}
	mounts := bootProgramMounts()
	if err := store.installBoot(program, mounts); err != nil {
		t.Fatal(err)
	}
	input := &bootProgramInput{}
	for range 3 {
		lease, err := store.materialize(t.Context(), program, input)
		if err != nil {
			t.Fatal(err)
		}
		if lease.mounts() != mounts {
			t.Fatalf("wrong mounts: %+v", lease.mounts())
		}
		if err := lease.close(); err != nil {
			t.Fatal(err)
		}
		if len(store.images) != 2 {
			t.Fatal("last Session removed boot mounts")
		}
	}
	if input.transfers != 0 {
		t.Fatal("boot Program transferred bytes")
	}
	if err := store.installBoot(proto.Clone(program).(*agentv1.SessionProgram), mounts); err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(program).(*agentv1.SessionProgram)
	changed.Artifact.Digest = sha256sum.DigestBytes([]byte("other"))
	if err := store.installBoot(changed, mounts); err == nil {
		t.Fatal("boot declaration changed")
	}
	if err := store.installBoot(nil, mounts); err == nil {
		t.Fatal("boot declaration removed")
	}
	// A different Program can still share the exact boot Runtime.
	image, err := store.acquire(t.Context(), agentv1.SessionProgramRequest_KIND_RUNTIME, changed.Runtime, input)
	if err != nil || image.image.root != mounts.Runtime {
		t.Fatalf("shared Runtime: %v", err)
	}
	if err := store.release(image); err != nil {
		t.Fatal(err)
	}
	wrong := proto.Clone(program.Runtime).(*agentv1.SessionProgramArtifact)
	wrong.SizeBytes++
	if _, err := store.acquire(t.Context(), agentv1.SessionProgramRequest_KIND_RUNTIME, wrong, input); err == nil {
		t.Fatal("accepted descriptor mismatch")
	}
}
func TestBootProgramDeclarationRejectsMalformedOrLateInput(t *testing.T) {
	bad := testBootProgram()
	bad.Runtime = nil
	if err := new(agentProgramStore).installBoot(bad, bootProgramMounts()); err == nil {
		t.Fatal("accepted missing Runtime")
	}
	store := &agentProgramStore{}
	if err := store.installBoot(nil, programMounts{}); err != nil {
		t.Fatal(err)
	}
	if err := store.installBoot(testBootProgram(), bootProgramMounts()); err == nil {
		t.Fatal("added boot drive after declaration")
	}
}
