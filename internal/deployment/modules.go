package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ModuleMapFormatVersion = 0
	TypeScriptTransformer  = "helmr.typescript.v0"
	ModuleFormatESM        = ModuleFormat("module")
	ModuleFormatCommonJS   = ModuleFormat("commonjs")
	maxModuleCount         = 65536
	moduleKeyDomain        = "helmr.typescript-module.v0"
)

type ModuleFormat string

type Module struct {
	CodeDigest   string       `json:"codeDigest"`
	CodePath     string       `json:"codePath"`
	Format       ModuleFormat `json:"format"`
	Path         string       `json:"path"`
	SourceDigest string       `json:"sourceDigest"`
}

type ModuleMap struct {
	FormatVersion int      `json:"formatVersion"`
	Modules       []Module `json:"modules"`
	Transformer   string   `json:"transformer"`
}

func ParseModuleMap(raw []byte) (ModuleMap, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ModuleMap{}, fmt.Errorf("module map size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ModuleMap{}, fmt.Errorf("canonicalize module map: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ModuleMap{}, fmt.Errorf("module map is not RFC 8785 canonical JSON")
	}

	var moduleMap ModuleMap
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&moduleMap); err != nil {
		return ModuleMap{}, fmt.Errorf("decode module map: %w", err)
	}
	if err := ensureEOF(decoder, "module map"); err != nil {
		return ModuleMap{}, err
	}
	if err := ValidateModuleMap(moduleMap); err != nil {
		return ModuleMap{}, err
	}
	complete, err := CanonicalModuleMap(moduleMap)
	if err != nil {
		return ModuleMap{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ModuleMap{}, fmt.Errorf("module map does not match the complete canonical v0 shape")
	}
	return moduleMap, nil
}

func CanonicalModuleMap(moduleMap ModuleMap) ([]byte, error) {
	if err := ValidateModuleMap(moduleMap); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(moduleMap)
	if err != nil {
		return nil, fmt.Errorf("encode module map: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize module map: %w", err)
	}
	if len(canonical) > int(maxProgramFileSizeBytes) {
		return nil, fmt.Errorf("module map size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	return canonical, nil
}

func ValidateModuleMap(moduleMap ModuleMap) error {
	if moduleMap.FormatVersion != ModuleMapFormatVersion {
		return fmt.Errorf("module map formatVersion = %d, want %d", moduleMap.FormatVersion, ModuleMapFormatVersion)
	}
	if moduleMap.Transformer != TypeScriptTransformer {
		return fmt.Errorf("module map transformer = %q, want %q", moduleMap.Transformer, TypeScriptTransformer)
	}
	if moduleMap.Modules == nil {
		return fmt.Errorf("module map modules must be an array")
	}
	if len(moduleMap.Modules) > maxModuleCount {
		return fmt.Errorf("module map contains %d modules, maximum is %d", len(moduleMap.Modules), maxModuleCount)
	}
	for position, module := range moduleMap.Modules {
		if err := validateModule(module); err != nil {
			return fmt.Errorf("module map entry %d: %w", position, err)
		}
		if position > 0 && bytes.Compare([]byte(moduleMap.Modules[position-1].Path), []byte(module.Path)) >= 0 {
			return fmt.Errorf("module map entries are not in canonical path order at position %d", position)
		}
	}
	return nil
}

func validateModule(module Module) error {
	if !sha256DigestPattern.MatchString(module.SourceDigest) {
		return fmt.Errorf("sourceDigest is not a lowercase SHA-256 digest")
	}
	if !sha256DigestPattern.MatchString(module.CodeDigest) {
		return fmt.Errorf("codeDigest is not a lowercase SHA-256 digest")
	}
	if err := validateModulePath(module.Path); err != nil {
		return err
	}
	if module.Format != ModuleFormatESM && module.Format != ModuleFormatCommonJS {
		return fmt.Errorf("format %q is unsupported", module.Format)
	}
	switch {
	case strings.HasSuffix(module.Path, ".mts") && module.Format != ModuleFormatESM:
		return fmt.Errorf(".mts module must use module format")
	case strings.HasSuffix(module.Path, ".cts") && module.Format != ModuleFormatCommonJS:
		return fmt.Errorf(".cts module must use commonjs format")
	}
	wantCodePath := moduleCodePath(module.Path, module.Format)
	if module.CodePath != wantCodePath {
		return fmt.Errorf("codePath = %q, want %q", module.CodePath, wantCodePath)
	}
	return nil
}

func validateModulePath(path string) error {
	if path == "" || !utf8.ValidString(path) || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return fmt.Errorf("path %q is not a confined relative POSIX path", path)
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return fmt.Errorf("path %q contains a control character", path)
		}
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("path %q is not normalized", path)
		}
	}
	root, _, _ := strings.Cut(path, "/")
	if root == "helmr" || root == ".helmr" || root == "node_modules" {
		return fmt.Errorf("path %q uses reserved Deployment root %q", path, root)
	}
	if strings.HasSuffix(path, ".d.ts") || strings.HasSuffix(path, ".d.mts") || strings.HasSuffix(path, ".d.cts") {
		return fmt.Errorf("path %q is declaration-only", path)
	}
	if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".mts") && !strings.HasSuffix(path, ".cts") {
		return fmt.Errorf("path %q is not an admitted TypeScript module", path)
	}
	return nil
}

func moduleCodePath(path string, format ModuleFormat) string {
	hash := sha256.New()
	hash.Write([]byte(moduleKeyDomain))
	hash.Write([]byte{0})
	hash.Write([]byte(path))
	extension := ".mjs"
	if format == ModuleFormatCommonJS {
		extension = ".cjs"
	}
	return "helmr/files/modules/" + hex.EncodeToString(hash.Sum(nil)) + extension
}
