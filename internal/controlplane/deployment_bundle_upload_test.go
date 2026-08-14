package controlplane

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestPlanDeploymentBundleUploadsRequiresOwnerProofBeforeSkipping(t *testing.T) {
	raw, bundle := controlPlaneDeploymentBundle(t)
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &bundleUploadStoreFixture{
		objects: map[string]cas.Object{
			bundle.Runtime.Artifact.Digest: bundleCASObject(bundle.Runtime.Artifact),
			bundle.Objects[0].Digest:       bundleCASObject(bundle.Objects[0]),
		},
	}
	ownership := &bundleOwnershipFixture{rows: map[string]db.CasObject{}}

	response, err := planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, bundle,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads: %v", err)
	}
	if response.BundleDigest == "" || len(response.Uploads) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if len(store.quarantined) != 1 || store.quarantined[0].MediaType != deployment.DeploymentBundleMediaType {
		t.Fatalf("quarantined = %+v", store.quarantined)
	}
	if store.presigned[0].Digest != bundle.Objects[0].Digest {
		t.Fatalf("presigned = %+v", store.presigned)
	}

	store.quarantine = map[string]cas.Descriptor{
		bundle.Objects[0].Digest: {
			Digest: bundle.Objects[0].Digest, SizeBytes: bundle.Objects[0].SizeBytes,
			MediaType: bundle.Objects[0].MediaType,
		},
	}
	store.presigned = nil
	response, err = planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, bundle,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads quarantine replay: %v", err)
	}
	if len(response.Uploads) != 0 || len(store.presigned) != 0 {
		t.Fatalf("quarantine response = %+v, presigned = %+v", response, store.presigned)
	}

	ownership.rows[bundle.Objects[0].Digest] = db.CasObject{
		OrgID: orgID, Digest: bundle.Objects[0].Digest,
		SizeBytes: bundle.Objects[0].SizeBytes, MediaType: bundle.Objects[0].MediaType,
	}
	store.presigned = nil
	response, err = planDeploymentBundleUploads(
		t.Context(), store, ownership, store, "0190-owner", orgID, raw, bundle,
	)
	if err != nil {
		t.Fatalf("planDeploymentBundleUploads owned replay: %v", err)
	}
	if len(response.Uploads) != 0 || len(store.presigned) != 0 {
		t.Fatalf("owned response = %+v, presigned = %+v", response, store.presigned)
	}
}

func TestPlanDeploymentBundleUploadsFailsClosedOnRuntimeOrOwnedObjectDrift(t *testing.T) {
	raw, bundle := controlPlaneDeploymentBundle(t)
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
	objects     map[string]cas.Object
	quarantine  map[string]cas.Descriptor
	quarantined []cas.Descriptor
	presigned   []cas.Descriptor
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

func (store *bundleUploadStoreFixture) PresignQuarantine(
	_ context.Context, _ string, expected cas.Descriptor, expires time.Duration,
) (cas.PresignedUpload, error) {
	if expires != deploymentBundleUploadExpiry {
		return cas.PresignedUpload{}, io.ErrUnexpectedEOF
	}
	store.presigned = append(store.presigned, expected)
	return cas.PresignedUpload{
		Method: "PUT", URL: "https://upload.invalid/" + expected.Digest,
		Headers: map[string]string{"Content-Type": expected.MediaType},
	}, nil
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

func bundleCASObject(object deployment.BundleObject) cas.Object {
	return cas.Object{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
}

func controlPlaneDeploymentBundle(t *testing.T) ([]byte, deployment.DeploymentBundle) {
	t.Helper()
	run := deployment.RunManifest{
		Queue: "default", MaxDurationMs: 5000,
		Retry: deployment.RetryManifest{Enabled: false},
	}
	task := deployment.TaskManifest{
		Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindNone}, Run: run,
	}
	declaration := deployment.ProgramIndexDeclaration{
		Kind: deployment.DefinitionKindTask, DeclaredID: "hello", Task: &task,
		Locator: &deployment.ProgramLocator{
			ExportName: "hello",
			ModulePath: ".helmr/modules/" + strings.Repeat("d", 64) + ".mjs",
			Slot:       deployment.DeclarationSlotHandler,
		},
	}
	plan := deployment.DeploymentPlan{
		FormatVersion: deployment.DeploymentPlanFormatVersion,
		Definitions:   []deployment.ProgramIndexDeclaration{declaration},
		Queues:        []deployment.QueueInput{{Name: "default"}},
	}
	programDigest := "sha256:" + strings.Repeat("a", 64)
	bundle := deployment.DeploymentBundle{
		Contract: deployment.DeploymentBundleContract,
		Platform: deployment.DeploymentBundlePlatform{
			Architecture: deployment.ArchitectureX8664, OS: deployment.DeploymentBundleTargetOS,
		},
		Plan: plan,
		Runtime: deployment.DeploymentBundleRuntime{
			Contract: deployment.RuntimeContract,
			Artifact: deployment.BundleObject{
				Digest: "sha256:" + strings.Repeat("f", 64), SizeBytes: 4096,
				MediaType: deployment.RuntimeArtifactMediaType,
			},
		},
		Program: deployment.ProgramOutput{
			Artifact: deployment.ProgramDescriptor{
				Digest: programDigest, SizeBytes: 4096, MediaType: deployment.ProgramArtifactMediaType,
			},
			Index: deployment.ProgramIndex{
				Architecture:       deployment.ArchitectureX8664,
				ConfigResultDigest: "sha256:" + strings.Repeat("c", 64),
				Declarations:       []deployment.ProgramIndexDeclaration{declaration},
				Queues:             plan.Queues,
				RuntimeContract:    deployment.RuntimeContract,
				RuntimeDigest:      "sha256:" + strings.Repeat("f", 64),
			},
		},
		WorkspaceImages: []deployment.BundleWorkspaceImage{},
		Objects: []deployment.BundleObject{{
			Digest: programDigest, SizeBytes: 4096, MediaType: deployment.ProgramArtifactMediaType,
		}},
	}
	raw, err := deployment.CanonicalDeploymentBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return raw, bundle
}

var _ deploymentBundleUploadStore = (*bundleUploadStoreFixture)(nil)
var _ deploymentBundleOwnershipStore = (*bundleOwnershipFixture)(nil)
