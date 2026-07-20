package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
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
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\\') ||
		strings.HasPrefix(value, "/") || path.Clean(value) != value ||
		value == "." || strings.HasPrefix(value, "../") ||
		strings.Contains(value, "/../") {
		return errors.New("must be a normalized project-root-relative POSIX path")
	}
	if value == "helmr" || strings.HasPrefix(value, "helmr/") {
		return errors.New("must not use the Platform-owned helmr root")
	}
	if hasNodeModulesComponent(value) {
		return errors.New("must not contain a node_modules component")
	}
	if strings.HasSuffix(value, ".d.ts") ||
		strings.HasSuffix(value, ".d.mts") ||
		strings.HasSuffix(value, ".d.cts") {
		return errors.New("must identify executable source, not a declaration file")
	}
	switch {
	case strings.HasSuffix(value, ".js"),
		strings.HasSuffix(value, ".mjs"),
		strings.HasSuffix(value, ".cjs"),
		strings.HasSuffix(value, ".ts"),
		strings.HasSuffix(value, ".mts"),
		strings.HasSuffix(value, ".cts"):
		return nil
	default:
		return errors.New("has an unsupported executable source suffix")
	}
}

func locatedDeclarationProjection(declaration LocatedDeclaration) ProgramDeclaration {
	projection := ProgramDeclaration{
		Kind:       declaration.Kind,
		DeclaredID: declaration.DeclaredID,
	}
	switch declaration.Kind {
	case DeclarationKindTask, DeclarationKindActor:
		projection.Slots = []DeclarationSlot{DeclarationSlotHandler}
	case DeclarationKindRunStream:
		projection.Slots = []DeclarationSlot{DeclarationSlotSchema}
	}
	return projection
}

func cloneDeclarationLocator(locator DeclarationLocator) DeclarationLocator {
	locator.Declarations = append([]LocatedDeclaration(nil), locator.Declarations...)
	return locator
}
