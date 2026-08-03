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

const DeclarationLocatorFormatVersion = 0

const ProgramEntry = `import { runProgram } from "file:///opt/helmr/runtime/helmr/entry.mjs";
await runProgram(new URL("./declarations.json", import.meta.url));
`

type DeclarationLocator struct {
	Declarations  []LocatedDeclaration `json:"declarations"`
	FormatVersion int                  `json:"formatVersion"`
}

type LocatedDeclaration struct {
	DeclaredID string          `json:"declaredId"`
	ExportName string          `json:"exportName"`
	Kind       DeclarationKind `json:"kind"`
	ModulePath string          `json:"modulePath"`
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
			"declaration locator does not match the complete canonical v0 shape",
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
	if err := validateDeclarationModulePath(declaration.ModulePath); err != nil {
		return fmt.Errorf("modulePath: %w", err)
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

func validateDeclarationModulePath(value string) error {
	const suffix = ".mjs"
	if err := validateArtifactPath(value, programArtifact); err != nil {
		return errors.New(
			"must identify <source-directory>/.helmr/modules/<64 lowercase hex>.mjs",
		)
	}
	components := strings.Split(value, "/")
	if len(components) < 3 ||
		components[len(components)-3] != ".helmr" ||
		components[len(components)-2] != "modules" ||
		!strings.HasSuffix(components[len(components)-1], suffix) {
		return errors.New(
			"must identify <source-directory>/.helmr/modules/<64 lowercase hex>.mjs",
		)
	}
	if slices.Contains(components[:len(components)-3], ".helmr") {
		return errors.New(
			"must identify <source-directory>/.helmr/modules/<64 lowercase hex>.mjs",
		)
	}
	name := strings.TrimSuffix(path.Base(value), suffix)
	if len(name) != 64 {
		return errors.New(
			"must identify <source-directory>/.helmr/modules/<64 lowercase hex>.mjs",
		)
	}
	for _, value := range name {
		if !('0' <= value && value <= '9') &&
			!('a' <= value && value <= 'f') {
			return errors.New(
				"must identify <source-directory>/.helmr/modules/<64 lowercase hex>.mjs",
			)
		}
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
