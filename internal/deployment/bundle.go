package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	DeploymentBundleContract  = "helmr.deployment-bundle.v0"
	DeploymentBundleMediaType = "application/vnd.helmr.deployment-bundle.v0+json"
	DeploymentBundleTargetOS  = "linux"

	MaxDeploymentBundleBytes           = 16 << 20
	MaxDeploymentBundleWorkspaceImages = 256
	MaxDeploymentBundleObjects         = 257
	MaxDeploymentBundleObjectBytes     = int64(4 << 30)
	MaxDeploymentBundleTotalBytes      = int64(4 << 30)
)

type DeploymentBundle struct {
	Contract        string                   `json:"contract"`
	Platform        DeploymentBundlePlatform `json:"platform"`
	Plan            DeploymentPlan           `json:"plan"`
	Runtime         DeploymentBundleRuntime  `json:"runtime"`
	Program         ProgramOutput            `json:"program"`
	WorkspaceImages []BundleWorkspaceImage   `json:"workspaceImages"`
	Objects         []BundleObject           `json:"objects"`
}

// DeploymentBundleAdmission is the exact Product release authority accepted by
// Control. Builder selection and dependency installation remain producer
// concerns; Control admits only the supported Runtime committed by the bundle.
type DeploymentBundleAdmission struct {
	Runtime RuntimeDescriptor
}

func (admission DeploymentBundleAdmission) Validate() error {
	if err := ValidateRuntimeDescriptor(admission.Runtime); err != nil {
		return fmt.Errorf("deployment bundle admission runtime: %w", err)
	}
	return nil
}

func (admission DeploymentBundleAdmission) Admit(bundle DeploymentBundle) error {
	if err := admission.Validate(); err != nil {
		return err
	}
	if err := ValidateDeploymentBundle(bundle); err != nil {
		return err
	}
	if bundle.Platform.Architecture != admission.Runtime.Architecture ||
		bundle.Runtime.Contract != admission.Runtime.RuntimeContract ||
		bundle.Runtime.Artifact.Digest != admission.Runtime.Digest ||
		bundle.Runtime.Artifact.SizeBytes != admission.Runtime.SizeBytes ||
		bundle.Runtime.Artifact.MediaType != admission.Runtime.MediaType {
		return errors.New("deployment bundle Runtime is not supported")
	}
	return nil
}

type DeploymentBundlePlatform struct {
	Architecture RuntimeArchitecture `json:"architecture"`
	OS           string              `json:"os"`
}

type DeploymentBundleRuntime struct {
	Contract string       `json:"contract"`
	Artifact BundleObject `json:"artifact"`
}

type BundleWorkspaceImage struct {
	DeclaredID string                       `json:"declaredId"`
	Artifact   BundleWorkspaceImageArtifact `json:"artifact"`
}

type BundleWorkspaceImageArtifact struct {
	Architecture RuntimeArchitecture `json:"architecture"`
	Digest       string              `json:"digest"`
	MediaType    string              `json:"mediaType"`
	SizeBytes    int64               `json:"sizeBytes"`
}

type BundleObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}

func ParseDeploymentBundle(raw []byte) (DeploymentBundle, error) {
	if len(raw) == 0 || len(raw) > MaxDeploymentBundleBytes {
		return DeploymentBundle{}, fmt.Errorf(
			"deployment bundle size is outside [1,%d]",
			MaxDeploymentBundleBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return DeploymentBundle{}, fmt.Errorf("canonicalize deployment bundle: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return DeploymentBundle{}, errors.New("deployment bundle is not RFC 8785 canonical JSON")
	}

	var bundle DeploymentBundle
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return DeploymentBundle{}, fmt.Errorf("decode deployment bundle: %w", err)
	}
	if err := ensureEOF(decoder, "deployment bundle"); err != nil {
		return DeploymentBundle{}, err
	}
	if err := ValidateDeploymentBundle(bundle); err != nil {
		return DeploymentBundle{}, err
	}
	complete, err := CanonicalDeploymentBundle(bundle)
	if err != nil {
		return DeploymentBundle{}, err
	}
	if !bytes.Equal(raw, complete) {
		return DeploymentBundle{}, errors.New(
			"deployment bundle does not match the complete canonical v0 shape",
		)
	}
	return bundle, nil
}

func CanonicalDeploymentBundle(bundle DeploymentBundle) ([]byte, error) {
	if err := ValidateDeploymentBundle(bundle); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode deployment bundle: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize deployment bundle: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > MaxDeploymentBundleBytes {
		return nil, fmt.Errorf(
			"deployment bundle size is outside [1,%d]",
			MaxDeploymentBundleBytes,
		)
	}
	return canonical, nil
}

func DeploymentBundleDigest(raw []byte) (string, error) {
	if _, err := ParseDeploymentBundle(raw); err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateDeploymentBundle(bundle DeploymentBundle) error {
	if bundle.Contract != DeploymentBundleContract {
		return fmt.Errorf(
			"deployment bundle contract = %q, want %q",
			bundle.Contract,
			DeploymentBundleContract,
		)
	}
	if bundle.Platform.OS != DeploymentBundleTargetOS {
		return fmt.Errorf(
			"deployment bundle platform os = %q, want %q",
			bundle.Platform.OS,
			DeploymentBundleTargetOS,
		)
	}
	if bundle.Platform.Architecture != ArchitectureX8664 {
		return fmt.Errorf(
			"deployment bundle platform architecture = %q, want %q",
			bundle.Platform.Architecture,
			ArchitectureX8664,
		)
	}
	if err := ValidateDeploymentPlan(bundle.Plan); err != nil {
		return fmt.Errorf("deployment bundle plan: %w", err)
	}
	if err := validateBundleRuntime(bundle.Runtime); err != nil {
		return err
	}
	if err := ValidateProgramOutput(bundle.Program); err != nil {
		return fmt.Errorf("deployment bundle program: %w", err)
	}
	if bundle.Program.Index.Architecture != bundle.Platform.Architecture {
		return errors.New("deployment bundle program architecture does not match platform")
	}
	if bundle.Program.Index.RuntimeContract != bundle.Runtime.Contract {
		return errors.New("deployment bundle program runtime contract does not match runtime")
	}
	if bundle.Program.Index.RuntimeDigest != bundle.Runtime.Artifact.Digest {
		return errors.New("deployment bundle program Runtime digest does not match runtime")
	}

	if _, err := validateBundleWorkspaceImages(bundle); err != nil {
		return err
	}
	if err := validateProgramIndexDeployment(bundle.Program.Index, bundle.Plan); err != nil {
		return fmt.Errorf("deployment bundle program index: %w", err)
	}
	return validateBundleObjectClosure(bundle)
}

func validateBundleRuntime(runtime DeploymentBundleRuntime) error {
	if runtime.Contract != RuntimeContract {
		return fmt.Errorf(
			"deployment bundle runtime contract = %q, want %q",
			runtime.Contract,
			RuntimeContract,
		)
	}
	if err := validateBundleObject(runtime.Artifact, "runtime"); err != nil {
		return err
	}
	if runtime.Artifact.MediaType != RuntimeArtifactMediaType {
		return fmt.Errorf(
			"deployment bundle runtime mediaType = %q, want %q",
			runtime.Artifact.MediaType,
			RuntimeArtifactMediaType,
		)
	}
	return nil
}

func validateBundleWorkspaceImages(bundle DeploymentBundle) ([]WorkspaceImage, error) {
	if bundle.WorkspaceImages == nil {
		return nil, errors.New("deployment bundle workspaceImages must be an array")
	}
	if len(bundle.WorkspaceImages) > MaxDeploymentBundleWorkspaceImages {
		return nil, fmt.Errorf(
			"deployment bundle has more than %d workspace images",
			MaxDeploymentBundleWorkspaceImages,
		)
	}
	sandboxes := deploymentPlanSandboxes(bundle.Plan)
	if len(bundle.WorkspaceImages) != len(sandboxes) {
		return nil, errors.New("deployment bundle workspaceImages do not match plan")
	}
	images := make([]WorkspaceImage, 0, len(bundle.WorkspaceImages))
	for index, image := range bundle.WorkspaceImages {
		if index > 0 && image.DeclaredID <= bundle.WorkspaceImages[index-1].DeclaredID {
			return nil, fmt.Errorf(
				"deployment bundle workspaceImages are not in canonical declaredId order at position %d",
				index,
			)
		}
		if image.DeclaredID != sandboxes[index].DeclaredID {
			return nil, fmt.Errorf(
				"deployment bundle workspaceImages[%d] declaredId does not match plan",
				index,
			)
		}
		if sandboxes[index].Sandbox == nil ||
			sandboxes[index].Sandbox.Image.ArtifactDigest != image.Artifact.Digest ||
			sandboxes[index].Sandbox.Image.MediaType != image.Artifact.MediaType {
			return nil, fmt.Errorf(
				"deployment bundle workspaceImages[%d] artifact does not match plan",
				index,
			)
		}
		artifact := image.Artifact
		if artifact.Architecture != bundle.Platform.Architecture {
			return nil, fmt.Errorf(
				"deployment bundle workspaceImages[%d] architecture does not match platform",
				index,
			)
		}
		object := BundleObject{
			Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		}
		if err := validateBundleObject(object, fmt.Sprintf("workspaceImages[%d]", index)); err != nil {
			return nil, err
		}
		if artifact.MediaType != WorkspaceImageArtifactMediaType {
			return nil, fmt.Errorf(
				"deployment bundle workspaceImages[%d] mediaType = %q, want %q",
				index,
				artifact.MediaType,
				WorkspaceImageArtifactMediaType,
			)
		}
		images = append(images, WorkspaceImage{
			DeclaredID: image.DeclaredID,
			Artifact: WorkspaceImageArtifact{
				Architecture: artifact.Architecture,
				Digest:       artifact.Digest,
				MediaType:    artifact.MediaType,
				SizeBytes:    artifact.SizeBytes,
			},
		})
	}
	return images, nil
}

func validateBundleObjectClosure(bundle DeploymentBundle) error {
	if bundle.Objects == nil {
		return errors.New("deployment bundle objects must be an array")
	}
	if len(bundle.Objects) > MaxDeploymentBundleObjects {
		return fmt.Errorf(
			"deployment bundle has more than %d objects",
			MaxDeploymentBundleObjects,
		)
	}
	expected := make(map[string]BundleObject, 1+len(bundle.WorkspaceImages))
	program := BundleObject{
		Digest:    bundle.Program.Artifact.Digest,
		SizeBytes: bundle.Program.Artifact.SizeBytes,
		MediaType: bundle.Program.Artifact.MediaType,
	}
	expected[program.Digest] = program
	for _, image := range bundle.WorkspaceImages {
		object := BundleObject{
			Digest:    image.Artifact.Digest,
			SizeBytes: image.Artifact.SizeBytes,
			MediaType: image.Artifact.MediaType,
		}
		if _, exists := expected[object.Digest]; exists {
			return fmt.Errorf("deployment bundle object digest %q is referenced more than once", object.Digest)
		}
		expected[object.Digest] = object
	}
	if len(bundle.Objects) != len(expected) {
		return errors.New("deployment bundle objects do not match the referenced closure")
	}

	var total int64
	for index, object := range bundle.Objects {
		if err := validateBundleObject(object, fmt.Sprintf("objects[%d]", index)); err != nil {
			return err
		}
		if index > 0 && object.Digest <= bundle.Objects[index-1].Digest {
			return fmt.Errorf(
				"deployment bundle objects are not in canonical digest order at position %d",
				index,
			)
		}
		reference, exists := expected[object.Digest]
		if !exists || reference != object {
			return fmt.Errorf("deployment bundle object %q is missing, extra, or conflicts with its reference", object.Digest)
		}
		if object.SizeBytes > MaxDeploymentBundleTotalBytes-total {
			return fmt.Errorf(
				"deployment bundle object closure exceeds %d bytes",
				MaxDeploymentBundleTotalBytes,
			)
		}
		total += object.SizeBytes
	}
	return nil
}

func validateBundleObject(object BundleObject, name string) error {
	if !sha256DigestPattern.MatchString(object.Digest) {
		return fmt.Errorf("deployment bundle %s digest is not a lowercase SHA-256 digest", name)
	}
	if object.SizeBytes < 1 || object.SizeBytes > MaxDeploymentBundleObjectBytes {
		return fmt.Errorf(
			"deployment bundle %s sizeBytes is outside [1,%d]",
			name,
			MaxDeploymentBundleObjectBytes,
		)
	}
	if object.MediaType != ProgramArtifactMediaType &&
		object.MediaType != WorkspaceImageArtifactMediaType &&
		object.MediaType != RuntimeArtifactMediaType {
		return fmt.Errorf("deployment bundle %s mediaType %q is unsupported", name, object.MediaType)
	}
	return nil
}

func SortDeploymentBundleObjects(objects []BundleObject) {
	sort.Slice(objects, func(left, right int) bool {
		return objects[left].Digest < objects[right].Digest
	})
}
