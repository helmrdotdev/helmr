package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const ComputerPreparationSpecVersion = "helmr.computer-preparation.v1"

// ComputerPreparationSpec binds the complete read-only Program used by one
// Computer's preparation. The Program commits its executable index, Runtime, seed
// configuration and stable build-Secret bindings. Actual Secret versions belong
// to an attempt's exposure evidence, not this immutable content identity.
//
// Preparation can read generated metadata as well as application files, so a
// payload-only digest is insufficient. Changes to any shipped Program bytes may
// require preparation again, even if the authored prepare function is unchanged.
type ComputerPreparationSpec struct {
	APIVersion           string            `json:"apiVersion"`
	ComputerDefinitionID string            `json:"computerDefinitionId"`
	Program              ProgramDescriptor `json:"program"`
}

// BuildComputerPreparationSpecs derives candidate inputs from a bundle's Program.
// Callers must verify that exact Program against its metadata before persisting or
// executing these candidates. This function does not attest artifact bytes.
func BuildComputerPreparationSpecs(output ProgramOutput) ([]ComputerPreparationSpec, error) {
	if err := ValidateProgramOutput(output); err != nil {
		return nil, err
	}
	specs := make([]ComputerPreparationSpec, 0)
	for _, d := range output.Metadata.Definitions {
		if d.Kind != definition.KindComputer {
			continue
		}
		specs = append(specs, ComputerPreparationSpec{
			APIVersion: ComputerPreparationSpecVersion, ComputerDefinitionID: d.DeclaredID,
			Program: output.Artifact,
		})
	}
	return specs, nil
}

func CanonicalComputerPreparationSpec(spec ComputerPreparationSpec) ([]byte, error) {
	if spec.APIVersion != ComputerPreparationSpecVersion || !definition.ValidDeclaredID(spec.ComputerDefinitionID) {
		return nil, errors.New("invalid Computer preparation identity")
	}
	if err := validateProgramDescriptor(spec.Program, "preparation Program", ProgramArtifactMediaType, MaxProgramPhysicalBytes); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

// ParseComputerPreparationSpec accepts a JSONB round trip while rejecting unknown
// fields, duplicate keys and incomplete shapes before deriving the canonical hash.
func ParseComputerPreparationSpec(raw []byte) (ComputerPreparationSpec, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return ComputerPreparationSpec{}, errors.New("computer preparation spec size is invalid")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ComputerPreparationSpec{}, err
	}
	var spec ComputerPreparationSpec
	if err := decodeClosedDefinition(canonical, &spec); err != nil {
		return ComputerPreparationSpec{}, err
	}
	complete, err := CanonicalComputerPreparationSpec(spec)
	if err != nil {
		return ComputerPreparationSpec{}, err
	}
	if !bytes.Equal(complete, canonical) {
		return ComputerPreparationSpec{}, errors.New("computer preparation spec has incomplete shape")
	}
	return spec, nil
}

func ComputerPreparationSpecDigest(spec ComputerPreparationSpec) (string, error) {
	raw, err := CanonicalComputerPreparationSpec(spec)
	if err != nil {
		return "", fmt.Errorf("computer preparation spec: %w", err)
	}
	return sha256sum.DigestBytes(raw), nil
}
