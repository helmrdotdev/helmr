package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

type managerResolverFunc func(
	context.Context,
	deployment.ManagerSelector,
) (deployment.ManagerCapsule, error)

func (resolve managerResolverFunc) Resolve(
	ctx context.Context,
	selector deployment.ManagerSelector,
) (deployment.ManagerCapsule, error) {
	return resolve(ctx, selector)
}

func TestValidateManagerAuthority(t *testing.T) {
	capsule := controlManagerCapsule()
	digest, err := deployment.ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, selection := managerSourceSelection(capsule)
	target, provenance := managerAuthorityInput(capsule, digest)
	provenance.Submitted = deployment.ProgramSubmittedSource{
		LockfileDigest: selection.LockfileDigest,
		LockfileName:   selection.LockfileName,
		SourceDigest:   sourceDigest,
	}
	resolver := managerResolverFunc(func(
		_ context.Context,
		selector deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		if selector != deployment.NewManagerSelector(
			capsule.PackageManager,
			capsule.Architecture,
		) {
			t.Fatalf("selector = %+v", selector)
		}
		return capsule, nil
	})

	if err := validateManagerAuthority(
		context.Background(),
		resolver,
		sourceDigest,
		selection,
		target,
		provenance,
	); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManagerAuthorityRejectsReceiptDigest(t *testing.T) {
	capsule := controlManagerCapsule()
	sourceDigest, selection := managerSourceSelection(capsule)
	target, provenance := managerAuthorityInput(capsule, "sha256:"+strings.Repeat("9", 64))
	provenance.Submitted = deployment.ProgramSubmittedSource{
		LockfileDigest: selection.LockfileDigest,
		LockfileName:   selection.LockfileName,
		SourceDigest:   sourceDigest,
	}
	resolver := managerResolverFunc(func(
		context.Context,
		deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		return capsule, nil
	})

	err := validateManagerAuthority(
		context.Background(),
		resolver,
		sourceDigest,
		selection,
		target,
		provenance,
	)
	if !errors.Is(err, errManagerAuthorityMismatch) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateManagerAuthorityPreservesStoreFailure(t *testing.T) {
	want := errors.New("manager store unavailable")
	resolver := managerResolverFunc(func(
		context.Context,
		deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		return deployment.ManagerCapsule{}, want
	})

	sourceDigest, selection := managerSourceSelection(controlManagerCapsule())
	target, provenance := managerAuthorityInput(
		controlManagerCapsule(),
		"sha256:"+strings.Repeat("2", 64),
	)
	provenance.Submitted = deployment.ProgramSubmittedSource{
		LockfileDigest: selection.LockfileDigest,
		LockfileName:   selection.LockfileName,
		SourceDigest:   sourceDigest,
	}
	err := validateManagerAuthority(
		context.Background(),
		resolver,
		sourceDigest,
		selection,
		target,
		provenance,
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if errors.Is(err, errManagerAuthorityMismatch) {
		t.Fatalf("store failure classified as receipt mismatch: %v", err)
	}
}

func TestValidateManagerAuthorityRejectsSubmittedSourceDivergence(t *testing.T) {
	capsule := controlManagerCapsule()
	digest, err := deployment.ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, selection := managerSourceSelection(capsule)
	target, provenance := managerAuthorityInput(capsule, digest)
	provenance.Submitted = deployment.ProgramSubmittedSource{
		LockfileDigest: selection.LockfileDigest,
		LockfileName:   selection.LockfileName,
		SourceDigest:   sourceDigest,
	}
	resolver := managerResolverFunc(func(
		context.Context,
		deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		return capsule, nil
	})
	provenance.Submitted.LockfileDigest = "sha256:" + strings.Repeat("9", 64)
	err = validateManagerAuthority(
		context.Background(),
		resolver,
		sourceDigest,
		selection,
		target,
		provenance,
	)
	if !errors.Is(err, errManagerAuthorityMismatch) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateManagerAuthorityRejectsFixedAuthorityDivergence(t *testing.T) {
	capsule := controlManagerCapsule()
	digest, err := deployment.ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, selection := managerSourceSelection(capsule)
	resolver := managerResolverFunc(func(
		context.Context,
		deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		return capsule, nil
	})

	tests := []struct {
		name   string
		mutate func(*deployment.BuildProvenance)
	}{
		{
			name: "architecture",
			mutate: func(provenance *deployment.BuildProvenance) {
				provenance.Architecture = "aarch64"
			},
		},
		{
			name: "build contract",
			mutate: func(provenance *deployment.BuildProvenance) {
				provenance.BuildContractVersion = "helmr.program-build.v1"
			},
		},
		{
			name: "runtime",
			mutate: func(provenance *deployment.BuildProvenance) {
				provenance.RuntimeDigest = "sha256:" + strings.Repeat("7", 64)
			},
		},
		{
			name: "toolchain",
			mutate: func(provenance *deployment.BuildProvenance) {
				provenance.StandardToolchainDigest = "sha256:" + strings.Repeat("8", 64)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, provenance := managerAuthorityInput(capsule, digest)
			provenance.Submitted = deployment.ProgramSubmittedSource{
				LockfileDigest: selection.LockfileDigest,
				LockfileName:   selection.LockfileName,
				SourceDigest:   sourceDigest,
			}
			test.mutate(&provenance)

			err := validateManagerAuthority(
				context.Background(),
				resolver,
				sourceDigest,
				selection,
				target,
				provenance,
			)
			if !errors.Is(err, errManagerAuthorityMismatch) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDeploymentDefinitionManifestProjectsWorkspaceImage(t *testing.T) {
	definition := deployment.DefinitionInput{
		Kind:       deployment.DefinitionKindWorkspace,
		DeclaredID: "workspace",
		Workspace: &deployment.WorkspaceInputManifest{
			ImageBuild: builder.ImageBuild{
				FormatVersion: builder.ImageBuildFormatVersion,
				Root:          "workspace",
				Images: []builder.ImageSpec{{
					Key: "workspace",
					Platform: builder.ImagePlatform{
						OS:           "linux",
						Architecture: "x86_64",
					},
					Steps: []builder.ImageStep{{
						From: &builder.ImageFrom{Ref: "alpine:3.23"},
					}},
				}},
			},
			Resources: deployment.ResourcesManifest{
				MilliCPU:  1000,
				MemoryMiB: 1024,
				DiskMiB:   8192,
			},
			Network: deployment.NetworkManifest{
				Internet:  true,
				DenyCIDRs: []string{},
			},
			Architecture: deployment.ArchitectureX8664,
		},
	}
	image := deployment.WorkspaceImageArtifact{
		Digest:       "sha256:" + strings.Repeat("a", 64),
		SizeBytes:    4096,
		MediaType:    deployment.WorkspaceImageArtifactMediaType,
		Architecture: deployment.ArchitectureX8664,
	}

	manifest, digest, err := deploymentDefinitionManifest(definition, &image)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"architecture":"x86_64","image":{"artifactDigest":"sha256:` +
		strings.Repeat("a", 64) +
		`","mediaType":"application/vnd.helmr.workspace-image.v0.oci-tar"},` +
		`"network":{"denyCidrs":[],"internet":true},` +
		`"resources":{"diskMiB":8192,"memoryMiB":1024,"milliCpu":1000}}`
	if string(manifest) != want {
		t.Fatalf("manifest = %s", manifest)
	}
	_, wantDigest, err := deployment.CanonicalManifestAndDigest([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	if digest != wantDigest {
		t.Fatalf("digest = %x, want %x", digest, wantDigest)
	}
	if strings.Contains(string(manifest), "imageBuild") {
		t.Fatalf("persisted manifest contains plan-only imageBuild: %s", manifest)
	}
}

func managerAuthorityInput(
	capsule deployment.ManagerCapsule,
	digest string,
) (deployment.BuildTarget, deployment.BuildProvenance) {
	runtimeDigest := "sha256:" + strings.Repeat("5", 64)
	toolchainDigest := "sha256:" + strings.Repeat("6", 64)
	target := deployment.BuildTarget{
		Runtime: deployment.RuntimeDescriptor{
			Architecture: capsule.Architecture,
			Digest:       runtimeDigest,
		},
		StandardToolchainDigest: toolchainDigest,
		BuildContractVersion:    deployment.ProgramBuildContractVersion,
	}
	return target, deployment.BuildProvenance{
		Architecture:         capsule.Architecture,
		BuildContractVersion: deployment.ProgramBuildContractVersion,
		Manager: deployment.ProgramManager{
			CapsuleDigest: digest,
			Name:          capsule.PackageManager.Name,
			Version:       capsule.PackageManager.Version,
		},
		RuntimeDigest:           runtimeDigest,
		StandardToolchainDigest: toolchainDigest,
	}
}

func managerSourceSelection(
	capsule deployment.ManagerCapsule,
) (string, deployment.SourceSelection) {
	return "sha256:" + strings.Repeat("3", 64), deployment.SourceSelection{
		Manager:        capsule.PackageManager,
		LockfileName:   "bun.lock",
		LockfileDigest: "sha256:" + strings.Repeat("4", 64),
	}
}

func controlManagerCapsule() deployment.ManagerCapsule {
	return deployment.ManagerCapsule{
		Architecture: deployment.ArchitectureX8664,
		Entrypoint: deployment.ManagerEntrypoint{
			Kind: deployment.ManagerEntrypointNative,
			Path: "/opt/helmr/manager/bin/bun",
		},
		FormatVersion: deployment.ManagerCapsuleFormatVersion,
		PackageManager: deployment.PackageManager{
			Name:    deployment.PackageManagerBun,
			Version: "1.3.10",
		},
		Source: deployment.ManagerSource{
			Digest: "sha256:" + strings.Repeat("1", 64),
			Origin: "https://github.com/oven-sh/bun/releases/download/" +
				"bun-v1.3.10/bun-linux-x64-baseline.zip",
			SizeBytes: 1,
		},
		Tree: deployment.ManagerArtifact{
			Digest:    "sha256:" + strings.Repeat("2", 64),
			MediaType: deployment.ManagerTreeMediaType,
			SizeBytes: 1,
		},
	}
}
