package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	maxBuildConfigBytes   = 1 << 20
	maxBuildConfigEntries = 10000
)

var configExtglob = regexp.MustCompile(`[?*+@!]\(`)

type BuildConfig struct {
	Dirs           []string `json:"dirs"`
	IgnorePatterns []string `json:"ignorePatterns"`
}

func ReadBuildConfigFrame(reader io.Reader) (BuildConfig, error) {
	raw, err := frameio.ReadMessageFrameBounded(reader, maxBuildConfigBytes)
	if err != nil {
		return BuildConfig{}, fmt.Errorf("read config result frame: %w", err)
	}
	var trailing [1]byte
	if _, err := io.ReadFull(reader, trailing[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			return BuildConfig{}, errors.New("config result channel contains trailing data")
		}
		return BuildConfig{}, fmt.Errorf("check config result channel trailing data: %w", err)
	}
	return ParseBuildConfig(raw)
}

func ParseBuildConfig(raw []byte) (BuildConfig, error) {
	if len(raw) == 0 || len(raw) > maxBuildConfigBytes {
		return BuildConfig{}, fmt.Errorf(
			"config result size is outside [1,%d]",
			maxBuildConfigBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return BuildConfig{}, fmt.Errorf("canonicalize config result: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return BuildConfig{}, errors.New("config result is not RFC 8785 canonical JSON")
	}
	var config BuildConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return BuildConfig{}, fmt.Errorf("decode config result: %w", err)
	}
	if err := ensureEOF(decoder, "config result"); err != nil {
		return BuildConfig{}, err
	}
	if err := ValidateBuildConfig(config); err != nil {
		return BuildConfig{}, err
	}
	complete, err := CanonicalBuildConfig(config)
	if err != nil {
		return BuildConfig{}, err
	}
	if !bytes.Equal(raw, complete) {
		return BuildConfig{}, errors.New(
			"config result does not match the complete canonical v0 shape",
		)
	}
	return cloneBuildConfig(config), nil
}

func CanonicalBuildConfig(config BuildConfig) ([]byte, error) {
	if err := ValidateBuildConfig(config); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode config result: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize config result: %w", err)
	}
	if len(canonical) > maxBuildConfigBytes {
		return nil, fmt.Errorf(
			"config result size is outside [1,%d]",
			maxBuildConfigBytes,
		)
	}
	return canonical, nil
}

func BuildConfigDigest(config BuildConfig) (string, error) {
	raw, err := CanonicalBuildConfig(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func ValidateBuildConfig(config BuildConfig) error {
	if err := validateConfigStrings(
		config.Dirs,
		"dirs",
		true,
		validateConfigDirectory,
	); err != nil {
		return err
	}
	return validateConfigStrings(
		config.IgnorePatterns,
		"ignorePatterns",
		false,
		validateConfigIgnorePattern,
	)
}

func validateConfigStrings(
	values []string,
	name string,
	nonempty bool,
	validate func(string) error,
) error {
	if values == nil {
		return fmt.Errorf("config result %s must be an array", name)
	}
	if nonempty && len(values) == 0 {
		return fmt.Errorf("config result %s must be non-empty", name)
	}
	if len(values) > maxBuildConfigEntries {
		return fmt.Errorf("config result %s exceeds %d entries", name, maxBuildConfigEntries)
	}
	for index, value := range values {
		if err := validate(value); err != nil {
			return fmt.Errorf("config result %s[%d]: %w", name, index, err)
		}
		if index > 0 && bytes.Compare(
			[]byte(values[index-1]),
			[]byte(value),
		) >= 0 {
			return fmt.Errorf("config result %s is not in canonical unique UTF-8 order", name)
		}
	}
	return nil
}

func validateConfigDirectory(value string) error {
	if !validConfigText(value) ||
		value == "" ||
		strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, "./") ||
		strings.Contains(value, "\\") {
		return errors.New("entry must be a normalized root-relative POSIX path")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("entry must be a normalized root-relative POSIX path")
		}
	}
	return nil
}

func validateConfigIgnorePattern(value string) error {
	if !validConfigText(value) ||
		value == "" ||
		strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, "!") ||
		strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") ||
		strings.Contains(value, "\\") ||
		strings.ContainsAny(value, "[]{}") ||
		configExtglob.MatchString(value) {
		return errors.New("entry is outside the supported discovery glob grammar")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." ||
			strings.Contains(segment, "**") && segment != "**" {
			return errors.New("entry is outside the supported discovery glob grammar")
		}
	}
	return nil
}

func validConfigText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool {
		return r <= 0x1f || r >= 0x7f && r <= 0x9f
	})
}

func cloneBuildConfig(config BuildConfig) BuildConfig {
	config.Dirs = slices.Clone(config.Dirs)
	config.IgnorePatterns = slices.Clone(config.IgnorePatterns)
	return config
}
