package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// DefinitionIndex is the sole owner of executable export locations in a bundle.
type DefinitionIndex struct {
	APIVersion string                `json:"apiVersion"`
	Agents     []AgentBundleEntry    `json:"agents"`
	Computers  []ComputerBundleEntry `json:"computers"`
}
type AgentBundleEntry struct {
	ID                   string `json:"id"`
	ComputerDefinitionID string `json:"computerDefinitionId"`
	ModulePath           string `json:"modulePath"`
	ExportName           string `json:"exportName"`
}
type ComputerBundleEntry struct {
	ID           string `json:"id"`
	ModulePath   string `json:"modulePath"`
	ExportName   string `json:"exportName"`
	ThroughAgent bool   `json:"throughAgent"`
}

func ParseDefinitionIndex(raw []byte) (DefinitionIndex, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return DefinitionIndex{}, errors.New("definition index size is invalid")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return DefinitionIndex{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return DefinitionIndex{}, errors.New("definition index must be canonical JSON")
	}
	var index DefinitionIndex
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return DefinitionIndex{}, err
	}
	if err := jsoncanon.RequireEOF(decoder, "Definition index"); err != nil {
		return DefinitionIndex{}, err
	}
	if err := ValidateDefinitionIndex(index); err != nil {
		return DefinitionIndex{}, err
	}
	complete, err := CanonicalDefinitionIndex(index)
	if err != nil {
		return DefinitionIndex{}, err
	}
	if !bytes.Equal(raw, complete) {
		return DefinitionIndex{}, errors.New("definition index does not match the complete shape")
	}
	return index, nil
}
func CanonicalDefinitionIndex(index DefinitionIndex) ([]byte, error) {
	if err := ValidateDefinitionIndex(index); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	raw, err = jsoncanon.Transform(raw)
	if err == nil && len(raw) > int(MaxProgramFileSizeBytes) {
		return nil, errors.New("definition index size is invalid")
	}
	return raw, err
}
func ValidateDefinitionIndex(index DefinitionIndex) error {
	count := len(index.Agents) + len(index.Computers)
	if index.APIVersion != "helmr.definition-index.v1" || index.Agents == nil || index.Computers == nil || count < 1 || count > 10000 {
		return errors.New("invalid Definition index")
	}
	agents := make(map[string]struct{}, len(index.Agents))
	computers := make(map[string]struct{}, len(index.Computers))
	type location struct{ module, export, computer string }
	through := make(map[location]struct{}, len(index.Agents))
	for _, entry := range index.Agents {
		if !definition.ValidDeclaredID(entry.ID) || !definition.ValidDeclaredID(entry.ComputerDefinitionID) {
			return errors.New("invalid Agent bundle id")
		}
		if _, exists := agents[entry.ID]; exists {
			return errors.New("duplicate Agent bundle id")
		}
		if err := validateBundleExport(entry.ModulePath, entry.ExportName); err != nil {
			return err
		}
		agents[entry.ID] = struct{}{}
		through[location{entry.ModulePath, entry.ExportName, entry.ComputerDefinitionID}] = struct{}{}
	}
	for _, entry := range index.Computers {
		if !definition.ValidDeclaredID(entry.ID) {
			return errors.New("invalid Computer bundle id")
		}
		if _, exists := computers[entry.ID]; exists {
			return errors.New("duplicate Computer bundle id")
		}
		if err := validateBundleExport(entry.ModulePath, entry.ExportName); err != nil {
			return err
		}
		if entry.ThroughAgent {
			if _, exists := through[location{entry.ModulePath, entry.ExportName, entry.ID}]; !exists {
				return errors.New("computer bundle export has no matching Agent")
			}
		}
		computers[entry.ID] = struct{}{}
	}
	for _, entry := range index.Agents {
		if _, exists := computers[entry.ComputerDefinitionID]; !exists {
			return fmt.Errorf("agent %q has no Computer bundle entry", entry.ID)
		}
	}
	return nil
}
func validateBundleExport(modulePath, exportName string) error {
	if err := validateDeclarationModulePath(modulePath); err != nil {
		return err
	}
	if len(exportName) < 1 || len(exportName) > 256 || !utf8.ValidString(exportName) {
		return errors.New("invalid bundle export name")
	}
	for _, r := range exportName {
		if unicode.IsControl(r) {
			return errors.New("bundle export name contains a control character")
		}
	}
	return nil
}
