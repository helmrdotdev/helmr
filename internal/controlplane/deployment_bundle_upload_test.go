package controlplane

import (
	"context"
	"io"
	"sort"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestPlanDeploymentBundleUploadsRequiresOwnerProofBeforeSkipping(t *testing.T) {
	raw, manifest := controlPlaneDeploymentBundle(t)
	orgID := pgvalue.UUID(uuid.NewV7())
	store := &bundleUploadStoreFixture{
		objects: map[string]cas.Object{
			manifest.Runtime.Artifact.Digest: bundleCASObject(manifest.Runtime.Artifact),
			manifest.Objects[0].Digest:       bundleCASObject(manifest.Objects[0]),
		},
	}
	ownership := &bundleOwnershipFixture{rows: map[string]db.CasObject{}}

	response, err := planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, manifest,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads: %v", err)
	}
	if response.BundleDigest == "" || len(response.Uploads) != len(manifest.Objects) {
		t.Fatalf("response = %+v", response)
	}
	if len(store.quarantined) != 1 || store.quarantined[0].MediaType != bundle.MediaType {
		t.Fatalf("quarantined = %+v", store.quarantined)
	}
	if response.Uploads[0].Digest != manifest.Objects[0].Digest {
		t.Fatalf("uploads = %+v", response.Uploads)
	}

	store.quarantine = make(map[string]cas.Descriptor)
	for _, object := range manifest.Objects {
		store.quarantine[object.Digest] = cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
	}
	response, err = planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, manifest,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads quarantine replay: %v", err)
	}
	if len(response.Uploads) != 0 {
		t.Fatalf("quarantine response = %+v", response)
	}

	store.quarantine = nil
	for _, object := range manifest.Objects {
		ownership.rows[object.Digest] = db.CasObject{OrgID: orgID, Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
		store.objects[object.Digest] = cas.Object{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
	}
	response, err = planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, manifest,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads owned replay: %v", err)
	}
	if len(response.Uploads) != 0 {
		t.Fatalf("owned response = %+v", response)
	}
}

func TestPlanDeploymentBundleUploadsFailsClosedOnRuntimeOrOwnedObjectDrift(t *testing.T) {
	raw, bundle := controlPlaneDeploymentBundle(t)
	orgID := pgvalue.UUID(uuid.NewV7())
	for name, configure := range map[string]func(*bundleUploadStoreFixture, *bundleOwnershipFixture){
		"Runtime": func(store *bundleUploadStoreFixture, _ *bundleOwnershipFixture) {
			store.objects[bundle.Runtime.Artifact.Digest] = cas.Object{
				Digest: bundle.Runtime.Artifact.Digest, SizeBytes: bundle.Runtime.Artifact.SizeBytes + 1,
				MediaType: bundle.Runtime.Artifact.MediaType,
			}
		},
		"owned object": func(store *bundleUploadStoreFixture, ownership *bundleOwnershipFixture) {
			ownership.rows[bundle.Objects[0].Digest] = db.CasObject{
				OrgID: orgID, Digest: bundle.Objects[0].Digest,
				SizeBytes: bundle.Objects[0].SizeBytes + 1, MediaType: bundle.Objects[0].MediaType,
			}
		},
		"quarantine object": func(store *bundleUploadStoreFixture, _ *bundleOwnershipFixture) {
			store.quarantine = map[string]cas.Descriptor{
				bundle.Objects[0].Digest: {
					Digest: bundle.Objects[0].Digest, SizeBytes: bundle.Objects[0].SizeBytes + 1,
					MediaType: bundle.Objects[0].MediaType,
				},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &bundleUploadStoreFixture{objects: map[string]cas.Object{
				bundle.Runtime.Artifact.Digest: bundleCASObject(bundle.Runtime.Artifact),
				bundle.Objects[0].Digest:       bundleCASObject(bundle.Objects[0]),
			}}
			ownership := &bundleOwnershipFixture{rows: map[string]db.CasObject{}}
			configure(store, ownership)
			if _, err := planDeploymentBundleUploads(
				t.Context(), store, ownership, store, "0190-owner", orgID, raw, bundle,
			); err == nil {
				t.Fatal("planDeploymentBundleUploads returned nil error")
			}
		})
	}
}

type bundleUploadStoreFixture struct {
	cas.Store
	objects     map[string]cas.Object
	quarantine  map[string]cas.Descriptor
	quarantined []cas.Descriptor
}

func (store *bundleUploadStoreFixture) Stat(_ context.Context, digest string) (cas.Object, error) {
	object, ok := store.objects[digest]
	if !ok {
		return cas.Object{}, pgx.ErrNoRows
	}
	return object, nil
}

func (*bundleUploadStoreFixture) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, pgx.ErrNoRows
}

func (store *bundleUploadStoreFixture) PutQuarantine(
	_ context.Context, _ string, expected cas.Descriptor, body io.Reader,
) error {
	if _, err := io.ReadAll(body); err != nil {
		return err
	}
	store.quarantined = append(store.quarantined, expected)
	return nil
}

func (store *bundleUploadStoreFixture) HasExactQuarantine(
	_ context.Context, _ string, expected cas.Descriptor,
) (bool, error) {
	actual, ok := store.quarantine[expected.Digest]
	if !ok {
		return false, nil
	}
	if actual != expected {
		return false, cas.ErrDigestMismatch
	}
	return true, nil
}

func (store *bundleUploadStoreFixture) StreamQuarantine(ctx context.Context, owner string, expected cas.Descriptor, body io.Reader) error {
	return store.PutQuarantine(ctx, owner, expected, body)
}

func (store *bundleUploadStoreFixture) PromoteQuarantine(
	_ context.Context, _ string, expected cas.Descriptor,
) (cas.Object, error) {
	object := cas.Object{Digest: expected.Digest, SizeBytes: expected.SizeBytes, MediaType: expected.MediaType}
	if store.objects == nil {
		store.objects = make(map[string]cas.Object)
	}
	store.objects[expected.Digest] = object
	return object, nil
}

type bundleOwnershipFixture struct {
	rows map[string]db.CasObject
}

func (store *bundleOwnershipFixture) GetCasObject(
	_ context.Context, params db.GetCasObjectParams,
) (db.CasObject, error) {
	row, ok := store.rows[params.Digest]
	if !ok {
		return db.CasObject{}, pgx.ErrNoRows
	}
	return row, nil
}

func bundleCASObject(object bundle.Object) cas.Object {
	return cas.Object{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
}

func controlPlaneDeploymentBundle(t *testing.T, prepared ...bool) ([]byte, bundle.Manifest) {
	t.Helper()
	metadata := artifacttest.ProgramMetadata(t)
	if len(prepared) > 0 && prepared[0] {
		for i := range metadata.Definitions {
			if metadata.Definitions[i].Template != nil {
				metadata.Definitions[i].Template.Prepare = true
			}
		}
	}
	plan := bundle.Plan{FormatVersion: definition.DeploymentPlanFormatVersion, Definitions: metadata.Definitions}
	programDigest := "sha256:" + strings.Repeat("a", 64)
	manifest := bundle.Manifest{
		Contract: bundle.Contract,
		Platform: bundle.Platform{
			Architecture: definition.ArchitectureX8664, OS: bundle.TargetOS,
		},
		Plan: plan,
		Runtime: bundle.Runtime{
			Contract: definition.RuntimeContract,
			Artifact: bundle.Object{
				Digest: "sha256:" + strings.Repeat("f", 64), SizeBytes: 4096,
				MediaType: artifact.RuntimeArtifactMediaType,
			},
		},
		Program: artifact.ProgramOutput{
			Artifact: artifact.ProgramDescriptor{
				Digest: programDigest, SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType,
			},
			Metadata: metadata,
		},
		ComputerSeeds: []bundle.ComputerSeed{{DeclaredID: "repo", Artifact: bundle.ComputerSeedArtifact{Profile: definition.ComputerSeedProfile, Architecture: definition.ArchitectureX8664, Digest: "sha256:" + strings.Repeat("d", 64), MediaType: definition.ComputerSeedMediaType, SizeBytes: 4096}}},
		Objects: []bundle.Object{{
			Digest: programDigest, SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType,
		}, {Digest: "sha256:" + strings.Repeat("d", 64), MediaType: definition.ComputerSeedMediaType, SizeBytes: 4096}},
	}
	manifest.Preparations = artifacttest.PreparationOutputs(t, manifest.Program)
	for _, preparation := range manifest.Preparations {
		manifest.Objects = append(manifest.Objects, bundle.Object{Digest: preparation.Artifact.Digest, SizeBytes: preparation.Artifact.SizeBytes, MediaType: preparation.Artifact.MediaType})
	}
	sort.Slice(manifest.Objects, func(i, j int) bool { return manifest.Objects[i].Digest < manifest.Objects[j].Digest })
	raw, err := bundle.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return raw, manifest
}

var _ cas.UploadStore = (*bundleUploadStoreFixture)(nil)
var _ deploymentBundleOwnershipStore = (*bundleOwnershipFixture)(nil)
