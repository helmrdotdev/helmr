package guestd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestDependencyComponents(t *testing.T) {
	t.Parallel()

	artifact := func(value byte) deployment.ManagerArtifact {
		return deployment.ManagerArtifact{
			Digest:    "sha256:" + strings.Repeat(hex.EncodeToString([]byte{value}), sha256.Size),
			MediaType: "test",
			SizeBytes: 4096,
		}
	}
	project := artifact(4)
	offline := artifact(5)
	base := deployment.ManagerRequest{
		ManagerTree:       artifact(1),
		Runtime:           artifact(2),
		StandardToolchain: artifact(3),
	}
	type expectedComponent struct {
		device string
		name   string
		noexec bool
	}
	tests := []struct {
		name    string
		request deployment.ManagerRequest
		want    []expectedComponent
	}{
		{
			name:    "probe",
			request: base,
			want: []expectedComponent{
				{device: "/dev/vdc", name: "manager"},
				{device: "/dev/vdd", name: "runtime"},
				{device: "/dev/vde", name: "toolchain"},
			},
		},
		{
			name: "resolve",
			request: func() deployment.ManagerRequest {
				request := base
				request.Project = &project
				return request
			}(),
			want: []expectedComponent{
				{device: "/dev/vdc", name: "manager"},
				{device: "/dev/vdd", name: "runtime"},
				{device: "/dev/vde", name: "toolchain"},
				{device: "/dev/vdf", name: "project", noexec: true},
			},
		},
		{
			name: "lifecycle",
			request: func() deployment.ManagerRequest {
				request := base
				request.Project = &project
				request.OfflineStore = &offline
				return request
			}(),
			want: []expectedComponent{
				{device: "/dev/vdc", name: "manager"},
				{device: "/dev/vdd", name: "runtime"},
				{device: "/dev/vde", name: "toolchain"},
				{device: "/dev/vdf", name: "project", noexec: true},
				{device: "/dev/vdg", name: "offline-store", noexec: true},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			components := dependencyComponents(test.request)
			if len(components) != len(test.want) {
				t.Fatalf(
					"dependencyComponents() length = %d, want %d",
					len(components),
					len(test.want),
				)
			}
			for index, want := range test.want {
				got := components[index]
				if got.device != want.device ||
					got.name != want.name ||
					got.noexec != want.noexec {
					t.Fatalf("component %d = %#v, want %#v", index, got, want)
				}
			}
		})
	}
}

func TestDependencyResolveShapeIsExact(t *testing.T) {
	for _, test := range []struct {
		name    string
		devices []string
		resolve bool
		valid   bool
	}{
		{
			name:    "probe",
			devices: []string{"vde", "vda", "vdc", "vdb", "vdd"},
			valid:   true,
		},
		{
			name: "resolve",
			devices: []string{
				"vdf",
				"vda",
				"vdc",
				"vdb",
				"vde",
				"vdd",
			},
			resolve: true,
			valid:   true,
		},
		{
			name: "lifecycle",
			devices: []string{
				"vdg",
				"vda",
				"vdc",
				"vdb",
				"vdf",
				"vde",
				"vdd",
			},
			valid: true,
		},
		{
			name:    "missing",
			devices: []string{"vda", "vdb", "vdc", "vdd"},
		},
		{
			name: "extra",
			devices: []string{
				"vda",
				"vdb",
				"vdc",
				"vdd",
				"vde",
				"vdf",
				"vdg",
				"vdh",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolve, err := dependencyResolveShape(test.devices)
			if (err == nil) != test.valid {
				t.Fatalf("dependencyResolveShape() error = %v", err)
			}
			if resolve != test.resolve {
				t.Fatalf(
					"dependencyResolveShape() = %t, want %t",
					resolve,
					test.resolve,
				)
			}
		})
	}
}

func TestVerifyDependencyContent(t *testing.T) {
	t.Parallel()

	content := bytes.Repeat([]byte("helmr"), 4096)
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := verifyDependencyContent(
		context.Background(),
		bytes.NewReader(content),
		int64(len(content)),
		digest,
	); err != nil {
		t.Fatalf("verifyDependencyContent() error = %v", err)
	}

	t.Run("truncated", func(t *testing.T) {
		t.Parallel()

		err := verifyDependencyContent(
			context.Background(),
			bytes.NewReader(content[:len(content)-1]),
			int64(len(content)),
			digest,
		)
		if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("verifyDependencyContent() error = %v", err)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		t.Parallel()

		err := verifyDependencyContent(
			context.Background(),
			bytes.NewReader(content),
			int64(len(content)),
			"sha256:"+strings.Repeat("0", sha256.Size*2),
		)
		if err == nil || !strings.Contains(err.Error(), "dependency component digest =") {
			t.Fatalf("verifyDependencyContent() error = %v", err)
		}
	})

	t.Run("noncanonical digest", func(t *testing.T) {
		t.Parallel()

		err := verifyDependencyContent(
			context.Background(),
			bytes.NewReader(content),
			int64(len(content)),
			strings.ToUpper(digest),
		)
		if err == nil || !strings.Contains(err.Error(), "lowercase SHA-256") {
			t.Fatalf("verifyDependencyContent() error = %v", err)
		}
	})
}
