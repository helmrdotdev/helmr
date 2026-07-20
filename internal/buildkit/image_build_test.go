package buildkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/moby/buildkit/solver/pb"
	"google.golang.org/protobuf/proto"
)

func TestPlanDeclaredImageCopiesTheAddressedApplicationView(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git", "config"),
		[]byte("[core]\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	plan, err := planDeclaredImage(builder.ImageBuild{
		FormatVersion: builder.ImageBuildFormatVersion,
		Root:          "app",
		Images: []builder.ImageSpec{{
			Key:      "app",
			Platform: builder.ImagePlatform{OS: "linux", Architecture: "x86_64"},
			Steps: []builder.ImageStep{
				{From: &builder.ImageFrom{Ref: "debian:bookworm"}},
				{CopySourceFile: &builder.ImageCopySourceFile{
					Dst:  "/app/config",
					Path: ".git/config",
				}},
				{CopySourceDir: &builder.ImageCopySourceDir{
					Dst:  "/app/source",
					Path: ".",
				}},
			},
		}},
	}, root, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.LocalMounts) != 2 {
		t.Fatalf("local mounts = %d, want 2", len(plan.LocalMounts))
	}

	definition, err := plan.State.Marshal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range definition.Def {
		var operation pb.Op
		if err := proto.Unmarshal(raw, &operation); err != nil {
			t.Fatal(err)
		}
		source := operation.GetSource()
		if source == nil || !strings.HasPrefix(source.Identifier, "local://") {
			continue
		}
		for key := range source.Attrs {
			if strings.Contains(key, "exclude") {
				t.Fatalf("local source %q has exclusion attribute %q", source.Identifier, key)
			}
		}
	}
}
