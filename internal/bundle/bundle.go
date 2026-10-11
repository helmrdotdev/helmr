// Package bundle defines the deployment bundle format: its manifest, deployment
// plan, Computer image descriptors, object closure, limits and directory
// layout.
package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	Contract  = "helmr.deployment-bundle.v0"
	MediaType = "application/vnd.helmr.deployment-bundle.v0+json"
	TargetOS  = "linux"

	MaxBytes         = 16 << 20
	MaxComputerSeeds = 256
	maxObjects       = 257
	maxObjectBytes   = int64(4 << 30)
	maxTotalBytes    = int64(4 << 30)
)

type Manifest struct {
	Contract      string                 `json:"contract"`
	Platform      Platform               `json:"platform"`
	Plan          Plan                   `json:"plan"`
	Runtime       Runtime                `json:"runtime"`
	Program       artifact.ProgramOutput `json:"program"`
	ComputerSeeds []ComputerSeed         `json:"computerSeeds"`
	Objects       []Object               `json:"objects"`
}

// Admission is the exact Product release authority accepted by
// Control. Builder selection and dependency installation remain producer
// concerns; Control admits only the supported Runtime committed by the bundle.
type Admission struct {
	Runtime artifact.RuntimeDescriptor
}

func (admission Admission) Validate() error {
	if err := artifact.ValidateRuntimeDescriptor(admission.Runtime); err != nil {
		return fmt.Errorf("deployment bundle admission runtime: %w", err)
	}
	return nil
}

func (admission Admission) Admit(bundle Manifest) error {
	if err := admission.Validate(); err != nil {
		return err
	}
	if err := validate(bundle); err != nil {
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

type Platform struct {
	Architecture definition.RuntimeArchitecture `json:"architecture"`
	OS           string                         `json:"os"`
}

type Runtime struct {
	Contract string `json:"contract"`
	Artifact Object `json:"artifact"`
}

type ComputerSeed struct {
	DeclaredID string               `json:"declaredId"`
	Artifact   ComputerSeedArtifact `json:"artifact"`
}

type ComputerSeedArtifact struct {
	Profile      string                         `json:"profile"`
	Config       oci.RuntimeConfig              `json:"config"`
	Architecture definition.RuntimeArchitecture `json:"architecture"`
	Digest       string                         `json:"digest"`
	MediaType    string                         `json:"mediaType"`
	SizeBytes    int64                          `json:"sizeBytes"`
}

// Seed returns the resolved initial disk input for a Computer definition.
func (artifact ComputerSeedArtifact) Seed() definition.ComputerSeed {
	return definition.ComputerSeed{
		Profile:      artifact.Profile,
		Config:       artifact.Config,
		Architecture: artifact.Architecture,
		Digest:       artifact.Digest,
		MediaType:    artifact.MediaType,
		SizeBytes:    artifact.SizeBytes,
	}
}

type Object struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}

func Parse(raw []byte) (Manifest, error) {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return Manifest{}, fmt.Errorf(
			"deployment bundle size is outside [1,%d]",
			MaxBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("canonicalize deployment bundle: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return Manifest{}, errors.New("deployment bundle is not RFC 8785 canonical JSON")
	}

	var bundle Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return Manifest{}, fmt.Errorf("decode deployment bundle: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "deployment bundle"); err != nil {
		return Manifest{}, err
	}
	if err := validate(bundle); err != nil {
		return Manifest{}, err
	}
	complete, err := Canonical(bundle)
	if err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return Manifest{}, errors.New(
			"deployment bundle does not match the complete canonical v0 shape",
		)
	}
	return bundle, nil
}

func Canonical(bundle Manifest) ([]byte, error) {
	if err := validate(bundle); err != nil {
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
	if len(canonical) == 0 || len(canonical) > MaxBytes {
		return nil, fmt.Errorf(
			"deployment bundle size is outside [1,%d]",
			MaxBytes,
		)
	}
	return canonical, nil
}

func Digest(raw []byte) (string, error) {
	if _, err := Parse(raw); err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return sha256sum.FormatDigest(digest[:]), nil
}

func validate(bundle Manifest) error {
	if bundle.Contract != Contract {
		return fmt.Errorf(
			"deployment bundle contract = %q, want %q",
			bundle.Contract,
			Contract,
		)
	}
	if bundle.Platform.OS != TargetOS {
		return fmt.Errorf(
			"deployment bundle platform os = %q, want %q",
			bundle.Platform.OS,
			TargetOS,
		)
	}
	if bundle.Platform.Architecture != definition.ArchitectureX8664 {
		return fmt.Errorf(
			"deployment bundle platform architecture = %q, want %q",
			bundle.Platform.Architecture,
			definition.ArchitectureX8664,
		)
	}
	if err := validatePlan(bundle.Plan); err != nil {
		return fmt.Errorf("deployment bundle plan: %w", err)
	}
	if err := validateBundleRuntime(bundle.Runtime); err != nil {
		return err
	}
	if err := artifact.ValidateProgramOutput(bundle.Program); err != nil {
		return fmt.Errorf("deployment bundle program: %w", err)
	}
	if bundle.Program.Metadata.Architecture != bundle.Platform.Architecture {
		return errors.New("deployment bundle program architecture does not match platform")
	}
	if bundle.Program.Metadata.RuntimeContract != bundle.Runtime.Contract {
		return errors.New("deployment bundle program runtime contract does not match runtime")
	}
	if bundle.Program.Metadata.RuntimeDigest != bundle.Runtime.Artifact.Digest {
		return errors.New("deployment bundle program Runtime digest does not match runtime")
	}

	if err := validateBundleComputerSeeds(bundle); err != nil {
		return err
	}
	if err := validateProgramMetadataDeployment(bundle.Program.Metadata, bundle.Plan); err != nil {
		return fmt.Errorf("deployment bundle program metadata: %w", err)
	}
	return validateBundleObjectClosure(bundle)
}

func validateBundleRuntime(runtime Runtime) error {
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
	if runtime.Artifact.MediaType != artifact.RuntimeArtifactMediaType {
		return fmt.Errorf(
			"deployment bundle runtime mediaType = %q, want %q",
			runtime.Artifact.MediaType,
			artifact.RuntimeArtifactMediaType,
		)
	}
	return nil
}

func validateBundleComputerSeeds(bundle Manifest) error {
	if bundle.ComputerSeeds == nil {
		return errors.New("deployment bundle computerSeeds must be an array")
	}
	if len(bundle.ComputerSeeds) > MaxComputerSeeds {
		return fmt.Errorf(
			"deployment bundle has more than %d Computer seeds",
			MaxComputerSeeds,
		)
	}
	computers := deploymentPlanComputers(bundle.Plan)
	if len(bundle.ComputerSeeds) != len(computers) {
		return errors.New("deployment bundle computerSeeds do not match plan")
	}
	for index, image := range bundle.ComputerSeeds {
		if index > 0 && image.DeclaredID <= bundle.ComputerSeeds[index-1].DeclaredID {
			return fmt.Errorf(
				"deployment bundle computerSeeds are not in canonical declaredId order at position %d",
				index,
			)
		}
		if image.DeclaredID != computers[index].DeclaredID {
			return fmt.Errorf(
				"deployment bundle computerSeeds[%d] declaredId does not match plan",
				index,
			)
		}
		if computers[index].Computer == nil ||
			computers[index].Computer.Seed.ArtifactDigest != image.Artifact.Digest ||
			computers[index].Computer.Seed.MediaType != image.Artifact.MediaType ||
			computers[index].Computer.Seed.Profile != image.Artifact.Profile ||
			!reflect.DeepEqual(computers[index].Computer.Seed.Config, image.Artifact.Config) {
			return fmt.Errorf(
				"deployment bundle computerSeeds[%d] artifact does not match plan",
				index,
			)
		}
		artifact := image.Artifact
		if artifact.Architecture != bundle.Platform.Architecture {
			return fmt.Errorf(
				"deployment bundle computerSeeds[%d] architecture does not match platform",
				index,
			)
		}
		object := Object{
			Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		}
		if err := validateBundleObject(object, fmt.Sprintf("computerSeeds[%d]", index)); err != nil {
			return err
		}
		if artifact.Profile != definition.ComputerSeedProfile {
			return fmt.Errorf("deployment disk profile %q is unsupported", artifact.Profile)
		}
		if artifact.MediaType != definition.ComputerSeedMediaType {
			return fmt.Errorf(
				"deployment bundle computerSeeds[%d] mediaType = %q, want %q",
				index,
				artifact.MediaType,
				definition.ComputerSeedMediaType,
			)
		}
	}
	return nil
}

func validateBundleObjectClosure(bundle Manifest) error {
	if bundle.Objects == nil {
		return errors.New("deployment bundle objects must be an array")
	}
	if len(bundle.Objects) > maxObjects {
		return fmt.Errorf(
			"deployment bundle has more than %d objects",
			maxObjects,
		)
	}
	expected := make(map[string]Object, 1+len(bundle.ComputerSeeds))
	program := Object{
		Digest:    bundle.Program.Artifact.Digest,
		SizeBytes: bundle.Program.Artifact.SizeBytes,
		MediaType: bundle.Program.Artifact.MediaType,
	}
	expected[program.Digest] = program
	for _, image := range bundle.ComputerSeeds {
		object := Object{
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
		if object.SizeBytes > maxTotalBytes-total {
			return fmt.Errorf(
				"deployment bundle object closure exceeds %d bytes",
				maxTotalBytes,
			)
		}
		total += object.SizeBytes
	}
	return nil
}

func validateBundleObject(object Object, name string) error {
	if !sha256sum.ValidDigest(object.Digest) {
		return fmt.Errorf("deployment bundle %s digest is not a lowercase SHA-256 digest", name)
	}
	if object.SizeBytes < 1 || object.SizeBytes > maxObjectBytes {
		return fmt.Errorf(
			"deployment bundle %s sizeBytes is outside [1,%d]",
			name,
			maxObjectBytes,
		)
	}
	if object.MediaType != artifact.ProgramArtifactMediaType &&
		object.MediaType != definition.ComputerSeedMediaType &&
		object.MediaType != artifact.RuntimeArtifactMediaType {
		return fmt.Errorf("deployment bundle %s mediaType %q is unsupported", name, object.MediaType)
	}
	return nil
}

func SortObjects(objects []Object) {
	sort.Slice(objects, func(left, right int) bool {
		return objects[left].Digest < objects[right].Digest
	})
}
