package workerapi

import "github.com/helmrdotdev/helmr/internal/cas"

const (
	// StorageCAS is the organization content store.
	StorageCAS = "cas"
	// StoragePlatform holds released runtime artifacts.
	StoragePlatform = "platform"

	// MaxStorageParts bounds one multipart part grant request.
	MaxStorageParts = 32
)

// StorageScope names exactly one authority under which a Host asks for storage
// grants: a live allocation it holds, or a qualified preparation source the
// Control Plane offered to it. Grants never outlive that authority.
type StorageScope struct {
	Computer          *AllocationIdentity  `json:"computer,omitempty"`
	Preparation       *PreparationExecutor `json:"preparation,omitempty"`
	BootSeed          *BootSeedExecutor    `json:"boot_seed,omitempty"`
	PreparationSource *PreparationSourceID `json:"preparation_source,omitempty"`
}

// PreparationSourceID identifies one preparation source by its boot seed build.
type PreparationSourceID struct {
	EnvironmentID string `json:"environment_id"`
	BuildID       string `json:"build_id"`
}

type StorageReadRequest struct {
	Scope  StorageScope `json:"scope"`
	Store  string       `json:"store"`
	Digest string       `json:"digest"`
}

// StorageReadGrant authorizes complete and ranged GETs and a HEAD of one object.
type StorageReadGrant struct {
	Digest string            `json:"digest"`
	Get    cas.SignedRequest `json:"get"`
	Head   cas.SignedRequest `json:"head"`
}

type StorageWriteRequest struct {
	Scope  StorageScope `json:"scope"`
	Object CASObject    `json:"object"`
}

// StorageWriteResponse is exactly one of: the object already exists with
// verified bytes, a single create-only PUT, or a multipart upload whose parts,
// completion and abort are granted through the Control Plane.
type StorageWriteResponse struct {
	Exists bool               `json:"exists,omitempty"`
	Put    *cas.SignedRequest `json:"put,omitempty"`
	Upload *cas.Upload        `json:"upload,omitempty"`
}

type StoragePartsRequest struct {
	Scope    StorageScope `json:"scope"`
	Object   CASObject    `json:"object"`
	UploadID string       `json:"upload_id"`
	Parts    []int32      `json:"parts"`
}

type StoragePartsResponse struct {
	Parts []cas.SignedRequest `json:"parts"`
}

type StorageCompleteRequest struct {
	Scope    StorageScope        `json:"scope"`
	Object   CASObject           `json:"object"`
	UploadID string              `json:"upload_id"`
	Parts    []cas.CompletedPart `json:"parts"`
}

type StorageAbortRequest struct {
	Scope    StorageScope `json:"scope"`
	Object   CASObject    `json:"object"`
	UploadID string       `json:"upload_id"`
}

// CheckpointKeyRequest asks for a memory checkpoint's data key under the
// capturing or restoring Computer allocation.
type CheckpointKeyRequest struct {
	Identity     AllocationIdentity `json:"identity"`
	CheckpointID string             `json:"checkpoint_id"`
}

// BootSeedKeyRequest asks for a boot seed's data key under the seed allocation
// building it, a Computer allocation booting from it, or its preparation source.
type BootSeedKeyRequest struct {
	Scope   StorageScope `json:"scope"`
	BuildID string       `json:"build_id"`
}

// ObjectKey is plaintext data-key material; holders clear it after use.
type ObjectKey struct {
	Key []byte `json:"key"`
}
