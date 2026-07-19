package control

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	plan := deployment.DependencyPlan{
		Architecture:         capsule.Architecture,
		ManagerCapsuleDigest: digest,
		PackageManager:       capsule.PackageManager,
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
		plan,
	); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManagerAuthorityRejectsReceiptDigest(t *testing.T) {
	capsule := controlManagerCapsule()
	plan := deployment.DependencyPlan{
		Architecture:         capsule.Architecture,
		ManagerCapsuleDigest: "sha256:" + strings.Repeat("9", 64),
		PackageManager:       capsule.PackageManager,
	}
	resolver := managerResolverFunc(func(
		context.Context,
		deployment.ManagerSelector,
	) (deployment.ManagerCapsule, error) {
		return capsule, nil
	})

	err := validateManagerAuthority(context.Background(), resolver, plan)
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

	err := validateManagerAuthority(
		context.Background(),
		resolver,
		deployment.DependencyPlan{
			Architecture: deployment.ArchitectureX8664,
			PackageManager: deployment.PackageManager{
				Name:    deployment.PackageManagerBun,
				Version: "1.3.10",
			},
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if errors.Is(err, errManagerAuthorityMismatch) {
		t.Fatalf("store failure classified as receipt mismatch: %v", err)
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
