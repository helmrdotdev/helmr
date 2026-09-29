package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	DeploymentBundleContract  = "helmr.deployment-bundle.v0"
	DeploymentBundleMediaType = "application/vnd.helmr.deployment-bundle.v0+json"
	DeploymentBundleTargetOS  = "linux"

	MaxDeploymentBundleBytes          = 16 << 20
	MaxDeploymentBundleComputerImages = 256
	MaxDeploymentBundleObjects        = 257
	MaxDeploymentBundleObjectBytes    = int64(4 << 30)
	MaxDeploymentBundleTotalBytes     = int64(4 << 30)
)

type DeploymentBundle struct {
	Contract       string                   `json:"contract"`
	Platform       DeploymentBundlePlatform `json:"platform"`
	Plan           DeploymentPlan           `json:"plan"`
	Runtime        DeploymentBundleRuntime  `json:"runtime"`
	Program        ProgramOutput            `json:"program"`
	ComputerImages []BundleComputerImage    `json:"computerImages"`
	Objects        []BundleObject           `json:"objects"`
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
	Architecture definition.RuntimeArchitecture `json:"architecture"`
	OS           string                         `json:"os"`
}

type DeploymentBundleRuntime struct {
	Contract string       `json:"contract"`
	Artifact BundleObject `json:"artifact"`
}

type BundleComputerImage struct {
	DeclaredID string                      `json:"declaredId"`
	Artifact   BundleComputerImageArtifact `json:"artifact"`
}

type BundleComputerImageArtifact struct {
	Profile      string                         `json:"profile"`
	Config       oci.RuntimeConfig              `json:"config"`
	Architecture definition.RuntimeArchitecture `json:"architecture"`
	Digest       string                         `json:"digest"`
	MediaType    string                         `json:"mediaType"`
	SizeBytes    int64                          `json:"sizeBytes"`
}

// ComputerImage returns the seed image input a sandbox manifest is compiled against.
func (artifact BundleComputerImageArtifact) ComputerImage() definition.ComputerImage {
	return definition.ComputerImage{
		Profile:      artifact.Profile,
		Config:       artifact.Config,
		Architecture: artifact.Architecture,
		Digest:       artifact.Digest,
		MediaType:    artifact.MediaType,
		SizeBytes:    artifact.SizeBytes,
	}
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
	return sha256sum.FormatDigest(digest[:]), nil
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
	if bundle.Platform.Architecture != definition.ArchitectureX8664 {
		return fmt.Errorf(
			"deployment bundle platform architecture = %q, want %q",
			bundle.Platform.Architecture,
			definition.ArchitectureX8664,
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

	if err := validateBundleComputerImages(bundle); err != nil {
		return err
	}
	if err := validateProgramIndexDeployment(bundle.Program.Index, bundle.Plan); err != nil {
		return fmt.Errorf("deployment bundle program index: %w", err)
	}
	return validateBundleObjectClosure(bundle)
}

func validateBundleRuntime(runtime DeploymentBundleRuntime) error {
	if runtime.Contract != definition.RuntimeContract {
		return fmt.Errorf(
			"deployment bundle runtime contract = %q, want %q",
			runtime.Contract,
			definition.RuntimeContract,
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

func validateBundleComputerImages(bundle DeploymentBundle) error {
	if bundle.ComputerImages == nil {
		return errors.New("deployment bundle computerImages must be an array")
	}
	if len(bundle.ComputerImages) > MaxDeploymentBundleComputerImages {
		return fmt.Errorf(
			"deployment bundle has more than %d computer images",
			MaxDeploymentBundleComputerImages,
		)
	}
	sandboxes := deploymentPlanSandboxes(bundle.Plan)
	if len(bundle.ComputerImages) != len(sandboxes) {
		return errors.New("deployment bundle computerImages do not match plan")
	}
	for index, image := range bundle.ComputerImages {
		if index > 0 && image.DeclaredID <= bundle.ComputerImages[index-1].DeclaredID {
			return fmt.Errorf(
				"deployment bundle computerImages are not in canonical declaredId order at position %d",
				index,
			)
		}
		if image.DeclaredID != sandboxes[index].DeclaredID {
			return fmt.Errorf(
				"deployment bundle computerImages[%d] declaredId does not match plan",
				index,
			)
		}
		if sandboxes[index].Sandbox == nil ||
			sandboxes[index].Sandbox.Image.ArtifactDigest != image.Artifact.Digest ||
			sandboxes[index].Sandbox.Image.MediaType != image.Artifact.MediaType ||
			sandboxes[index].Sandbox.Image.Profile != image.Artifact.Profile ||
			!reflect.DeepEqual(sandboxes[index].Sandbox.Image.Config, image.Artifact.Config) {
			return fmt.Errorf(
				"deployment bundle computerImages[%d] artifact does not match plan",
				index,
			)
		}
		artifact := image.Artifact
		if artifact.Architecture != bundle.Platform.Architecture {
			return fmt.Errorf(
				"deployment bundle computerImages[%d] architecture does not match platform",
				index,
			)
		}
		object := BundleObject{
			Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		}
		if err := validateBundleObject(object, fmt.Sprintf("computerImages[%d]", index)); err != nil {
			return err
		}
		if artifact.Profile != computer.SeedProfile {
			return fmt.Errorf("deployment disk profile %q is unsupported", artifact.Profile)
		}
		if artifact.MediaType != ComputerImageArtifactMediaType {
			return fmt.Errorf(
				"deployment bundle computerImages[%d] mediaType = %q, want %q",
				index,
				artifact.MediaType,
				ComputerImageArtifactMediaType,
			)
		}
	}
	return nil
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
	expected := make(map[string]BundleObject, 1+len(bundle.ComputerImages))
	program := BundleObject{
		Digest:    bundle.Program.Artifact.Digest,
		SizeBytes: bundle.Program.Artifact.SizeBytes,
		MediaType: bundle.Program.Artifact.MediaType,
	}
	expected[program.Digest] = program
	for _, image := range bundle.ComputerImages {
		object := BundleObject{
			Digest:    image.Artifact.Digest,
			SizeBytes: image.Artifact.SizeBytes,
			MediaType: image.Artifact.MediaType,
		}
		if existing, exists := expected[object.Digest]; exists {
			if existing != object {
				return fmt.Errorf(
					"deployment bundle object digest %q has conflicting reference metadata",
					object.Digest,
				)
			}
			continue
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
		object.MediaType != ComputerImageArtifactMediaType &&
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
