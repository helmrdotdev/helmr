package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const DeclarationLocatorFormatVersion = 1

type DeclarationLocator struct {
	Declarations  []LocatedDeclaration `json:"declarations"`
	FormatVersion int                  `json:"formatVersion"`
}

type LocatedDeclaration struct {
	DeclaredID string          `json:"declaredId"`
	ExportName string          `json:"exportName"`
	Kind       DeclarationKind `json:"kind"`
	SourcePath string          `json:"sourcePath"`
	Slot       DeclarationSlot `json:"slot"`
}

func ParseDeclarationLocator(raw []byte) (DeclarationLocator, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return DeclarationLocator{}, fmt.Errorf(
			"declaration locator size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return DeclarationLocator{}, fmt.Errorf("canonicalize declaration locator: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return DeclarationLocator{}, errors.New(
			"declaration locator is not RFC 8785 canonical JSON",
		)
	}
	var locator DeclarationLocator
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&locator); err != nil {
		return DeclarationLocator{}, fmt.Errorf("decode declaration locator: %w", err)
	}
	if err := ensureEOF(decoder, "declaration locator"); err != nil {
		return DeclarationLocator{}, err
	}
	if err := ValidateDeclarationLocator(locator); err != nil {
		return DeclarationLocator{}, err
	}
	complete, err := CanonicalDeclarationLocator(locator)
	if err != nil {
		return DeclarationLocator{}, err
	}
	if !bytes.Equal(raw, complete) {
		return DeclarationLocator{}, errors.New(
			"declaration locator does not match the complete canonical v1 shape",
		)
	}
	return cloneDeclarationLocator(locator), nil
}

func CanonicalDeclarationLocator(locator DeclarationLocator) ([]byte, error) {
	if err := ValidateDeclarationLocator(locator); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(locator)
	if err != nil {
		return nil, fmt.Errorf("encode declaration locator: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize declaration locator: %w", err)
	}
	if len(canonical) > int(maxProgramFileSizeBytes) {
		return nil, fmt.Errorf(
			"declaration locator size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	return canonical, nil
}

func ValidateDeclarationLocator(locator DeclarationLocator) error {
	if locator.FormatVersion != DeclarationLocatorFormatVersion {
		return fmt.Errorf(
			"declaration locator formatVersion = %d, want %d",
			locator.FormatVersion,
			DeclarationLocatorFormatVersion,
		)
	}
	if len(locator.Declarations) == 0 {
		return errors.New("declaration locator declarations must not be empty")
	}
	for index, declaration := range locator.Declarations {
		if err := validateLocatedDeclaration(declaration); err != nil {
			return fmt.Errorf("declaration locator declaration %d: %w", index, err)
		}
		if index > 0 {
			previous := locator.Declarations[index-1]
			if compareDeclarations(
				locatedDeclarationProjection(previous),
				locatedDeclarationProjection(declaration),
			) >= 0 {
				return fmt.Errorf(
					"declarations are not in canonical order at position %d",
					index,
				)
			}
		}
	}
	return nil
}

func validateLocatedDeclaration(declaration LocatedDeclaration) error {
	if err := validateDeclaration(locatedDeclarationProjection(declaration)); err != nil {
		return err
	}
	if err := validateDeclarationSourcePath(declaration.SourcePath); err != nil {
		return fmt.Errorf("sourcePath: %w", err)
	}
	if declaration.Slot != DeclarationSlotHandler {
		return errors.New("slot must be handler")
	}
	if len(declaration.ExportName) == 0 || len([]byte(declaration.ExportName)) > 256 ||
		!utf8.ValidString(declaration.ExportName) {
		return errors.New("exportName must contain 1 to 256 valid UTF-8 bytes")
	}
	for _, value := range declaration.ExportName {
		if unicode.IsControl(value) {
			return errors.New("exportName must not contain control characters")
		}
	}
	return nil
}

func validateDeclarationSourcePath(value string) error {
	if err := validateArtifactPath(value, programArtifact); err != nil {
		return err
	}
	if value == "helmr.config.ts" || strings.HasPrefix(value, "helmr/") || hasNodeModulesComponent(value) ||
		!slices.Contains([]string{".js", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts", ".jsx"}, path.Ext(value)) ||
		strings.HasSuffix(value, ".d.ts") || strings.HasSuffix(value, ".d.mts") || strings.HasSuffix(value, ".d.cts") {
		return errors.New("must identify a project declaration source, outside node_modules and build-only config")
	}
	return nil
}

func locatedDeclarationProjection(declaration LocatedDeclaration) ProgramDeclaration {
	projection := ProgramDeclaration{
		Kind:       declaration.Kind,
		DeclaredID: declaration.DeclaredID,
	}
	switch declaration.Kind {
	case DeclarationKindTask, DeclarationKindActor:
		projection.Slots = []DeclarationSlot{DeclarationSlotHandler}
	}
	return projection
}

func cloneDeclarationLocator(locator DeclarationLocator) DeclarationLocator {
	locator.Declarations = append([]LocatedDeclaration(nil), locator.Declarations...)
	return locator
}
