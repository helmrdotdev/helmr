package computer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type generationTestPublication struct {
	remote     *cas.File
	registered map[string]blockformat.ObjectInspection
	certified  map[string]blockformat.ObjectInspection
	fail       string
}

func inspectionDescriptor(e blockformat.ObjectInspection) cas.Descriptor {
	if e.Segment != nil {
		return cas.Descriptor{Digest: digestOf(e.Segment.Digest), SizeBytes: e.Segment.Size, MediaType: "application/octet-stream"}
	}
	p := e.Pack.Pages[0].Locator.Pack
	return cas.Descriptor{Digest: digestOf(p.Digest), SizeBytes: p.Size, MediaType: "application/octet-stream"}
}
func (p *generationTestPublication) Register(_ context.Context, e blockformat.ObjectInspection) error {
	if e.Pack != nil {
		for _, page := range e.Pack.Pages {
			for _, child := range page.Children {
				c, ok := p.certified[digestOf(child.Locator.Pack.Digest)]
				if !ok || c.Pack == nil {
					return errors.New("child not certified")
				}
				if err := c.Pack.CheckNode(child); err != nil {
					return err
				}
			}
			for _, child := range page.Segments {
				c, ok := p.certified[digestOf(child.Digest)]
				if !ok || c.Segment == nil || *c.Segment != child {
					return errors.New("segment not certified")
				}
			}
		}
	}
	p.registered[inspectionDescriptor(e).Digest] = e
	if p.fail == "register" {
		p.fail = ""
		return errors.New("register response lost")
	}
	return nil
}
func (p *generationTestPublication) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if _, ok := p.registered[d.Digest]; !ok {
		return cas.Object{}, errors.New("upload before registration")
	}
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	stored, err := p.remote.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
	if err != nil {
		return cas.Object{}, err
	}
	if p.fail == "upload" {
		p.fail = ""
		return cas.Object{}, errors.New("upload response lost")
	}
	return stored, nil
}
func (p *generationTestPublication) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	d := inspectionDescriptor(e)
	o, err := p.remote.Stat(ctx, d.Digest)
	if err != nil || o.SizeBytes != d.SizeBytes {
		return errors.New("remote object missing")
	}
	p.certified[d.Digest] = e
	if p.fail == "certify" {
		p.fail = ""
		return errors.New("certification response lost")
	}
	return nil
}
func TestInitialGenerationMultiBatchAndPublicationRetry(t *testing.T) {
	for _, failure := range []string{"register", "upload", "certify"} {
		t.Run(failure, func(t *testing.T) {
			disk, err := os.CreateTemp(t.TempDir(), "disk")
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			const capacity = 9 << 20
			if err = disk.Truncate(capacity); err != nil {
				t.Fatal(err)
			}
			content := bytes.Repeat([]byte{7}, (4<<20)+4096)
			if _, err = disk.WriteAt(content, 0); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			key := bytes.Repeat([]byte{3}, 32)
			candidate, err := CaptureInitialGeneration(t.Context(), GenerationCapture{Disk: disk, Capacity: capacity, StagingParent: parent, Scope: "scope", KeyID: "key", Key: key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 64 << 20, MaxObjects: 1000})
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.Close()
			original := candidate.root
			remote, err := cas.NewFile(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			publisher := &generationTestPublication{remote: remote, registered: map[string]blockformat.ObjectInspection{}, certified: map[string]blockformat.ObjectInspection{}, fail: failure}
			root, err := candidate.Publish(t.Context(), publisher)
			if err == nil || root != (blockformat.Locator{}) {
				t.Fatal("uncertain publication returned root")
			}
			root, err = candidate.Publish(t.Context(), publisher)
			if err != nil || root != original {
				t.Fatalf("retry changed generation: %v", err)
			}
			roots := 0
			for _, e := range publisher.registered {
				if e.Pack != nil {
					for _, page := range e.Pack.Pages {
						if page.Locator.Page.Kind == blockformat.RootKind {
							roots++
						}
					}
				}
			}
			if roots != 1 {
				t.Fatalf("published private intermediate roots: %d", roots)
			}
			tree, err := blockformat.OpenTree(t.Context(), remote, "scope", map[string][]byte{"key": key}, root)
			if err != nil {
				t.Fatal(err)
			}
			for _, block := range []uint64{0, 1023, 1024, 1025, 2303} {
				b, err := tree.ReadBlock(t.Context(), block)
				want := byte(0)
				if block <= 1024 {
					want = 7
				}
				if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{want}, 4096)) {
					t.Fatalf("block %d: %v", block, err)
				}
			}
			if failure == "certify" {
				// The largest unaligned VM read spans 1,025 encryption blocks.
				data, err := tree.ReadRange(t.Context(), 1, blockformat.MaxReadBytes)
				if err != nil || !bytes.Equal(data, content[1:1+blockformat.MaxReadBytes]) {
					t.Fatalf("maximum unaligned generation range: %v", err)
				}
			}
			if err = candidate.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = candidate.Publish(t.Context(), publisher); !errors.Is(err, os.ErrClosed) {
				t.Fatal("closed candidate reused")
			}
			if !bytes.Equal(key, bytes.Repeat([]byte{3}, 32)) {
				t.Fatal("caller key modified")
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("local stage leaked")
			}
			if _, err = tree.ReadBlock(t.Context(), 0); err != nil {
				t.Fatal("close removed remote state")
			}
		})
	}
}
func TestInitialGenerationAdmissionAndCancellation(t *testing.T) {
	disk, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err = disk.Truncate(8192); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"budget", "capacity", "cancel"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			request := GenerationCapture{Disk: disk, Capacity: 8192, StagingParent: parent, Scope: "scope", KeyID: "key", Key: bytes.Repeat([]byte{1}, 32), Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 1 << 20, MaxObjects: 10}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "budget":
				request.MaxStagedBytes = 1
			case "capacity":
				request.Capacity = 4096
			case "cancel":
				cancel()
			}
			if c, err := CaptureInitialGeneration(ctx, request); err == nil || c != nil {
				t.Fatal("invalid capture admitted")
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("failed capture leaked staging")
			}
		})
	}
}

func digestOf(d [32]byte) string { return "sha256:" + hex.EncodeToString(d[:]) }
