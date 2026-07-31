package imagebuild

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	FormatVersion = 0

	maxImageIdentifierBytes  = 512
	maxImageReferenceBytes   = 4096
	maxImagePathBytes        = 4096
	maxImageArguments        = 1024
	maxImageArgumentBytes    = 65536
	maxImageArgumentsBytes   = 1 << 20
	maxEnvKeyBytes           = 256
	maxEnvValueBytes         = 1 << 20
	maxEnvBytes              = 1 << 20
	maxUserBytes             = 256
	maxRegistryUsernameBytes = 256
	maxRegistryAuthorities   = 8
	maxImageBuildBytes       = 16 << 20
	maxBuildSteps            = 10000
)

var (
	imageEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	imageUserPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?::[A-Za-z0-9_.-]+)?$`)
	secretNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

type Build struct {
	FormatVersion int    `json:"formatVersion"`
	Root          string `json:"root"`
	Images        []Spec `json:"images"`
}

type Spec struct {
	Key      string   `json:"key"`
	Platform Platform `json:"platform"`
	Steps    []Step   `json:"steps"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type Step struct {
	From           *From           `json:"from,omitempty"`
	Run            *Run            `json:"run,omitempty"`
	CopySourceFile *CopySourceFile `json:"copySourceFile,omitempty"`
	CopySourceDir  *CopySourceDir  `json:"copySourceDir,omitempty"`
	CopyFromImage  *CopyFromImage  `json:"copyFromImage,omitempty"`
	Workdir        *Workdir        `json:"workdir,omitempty"`
	User           *User           `json:"user,omitempty"`
	Env            *Env            `json:"env,omitempty"`
}

type From struct {
	Ref  string        `json:"ref"`
	Auth *RegistryAuth `json:"auth,omitempty"`
}

type RegistryAuth struct {
	Username       string `json:"username"`
	PasswordSecret string `json:"passwordSecret"`
}

type RegistryCredential struct {
	Authority      string
	Username       string
	PasswordSecret string
}

type Run struct {
	Argv []string `json:"argv"`
}

type CopySourceFile struct {
	Dst  string `json:"dst"`
	Path string `json:"path"`
}

type CopySourceDir struct {
	Dst  string `json:"dst"`
	Path string `json:"path"`
}

type CopyFromImage struct {
	Dst      string `json:"dst"`
	ImageKey string `json:"imageKey"`
	SrcPath  string `json:"srcPath"`
}

type Workdir struct {
	Path string `json:"path"`
}

type User struct {
	Name string `json:"name"`
}

type Env struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func Validate(build Build, architecture string) error {
	if build.FormatVersion != FormatVersion {
		return fmt.Errorf(
			"image build formatVersion = %d, want %d",
			build.FormatVersion,
			FormatVersion,
		)
	}
	if !validImageArchitecture(architecture) {
		return fmt.Errorf("image build architecture %q is unsupported", architecture)
	}
	if err := validateImageIdentifier(build.Root, "image build root"); err != nil {
		return err
	}
	if len(build.Images) == 0 {
		return errors.New("image build images must be non-empty")
	}

	images := make(map[string]int, len(build.Images))
	registryCredentials := make(map[string]RegistryCredential)
	totalSteps := 0
	for index := range build.Images {
		image := &build.Images[index]
		if err := validateImageIdentifier(image.Key, fmt.Sprintf("image %d key", index)); err != nil {
			return err
		}
		if index > 0 && build.Images[index-1].Key >= image.Key {
			return errors.New("image build images must be unique and sorted by key")
		}
		images[image.Key] = index
		if image.Platform.OS != "linux" {
			return fmt.Errorf("image %q platform.os = %q, want linux", image.Key, image.Platform.OS)
		}
		if image.Platform.Architecture != architecture {
			return fmt.Errorf(
				"image %q architecture = %q, want %q",
				image.Key,
				image.Platform.Architecture,
				architecture,
			)
		}
		if len(image.Steps) == 0 {
			return fmt.Errorf("image %q steps must be non-empty", image.Key)
		}
		totalSteps += len(image.Steps)
		if totalSteps > maxBuildSteps {
			return fmt.Errorf("image build has more than %d steps", maxBuildSteps)
		}
		if image.Steps[0].From == nil {
			return fmt.Errorf("image %q first step must be from", image.Key)
		}
		envBytes := 0
		for stepIndex := range image.Steps {
			step := image.Steps[stepIndex]
			if stepIndex > 0 && step.From != nil {
				return fmt.Errorf("image %q has more than one from step", image.Key)
			}
			addedEnvBytes, err := validateStep(step, image.Key, stepIndex)
			if err != nil {
				return err
			}
			if step.From != nil {
				if err := validateRegistryBinding(
					*step.From,
					fmt.Sprintf("image %q step %d", image.Key, stepIndex),
					registryCredentials,
				); err != nil {
					return err
				}
			}
			envBytes += addedEnvBytes
			if envBytes > maxEnvBytes {
				return fmt.Errorf("image %q environment exceeds %d bytes", image.Key, maxEnvBytes)
			}
		}
	}
	if _, ok := images[build.Root]; !ok {
		return fmt.Errorf("image build root %q does not name an image", build.Root)
	}

	visiting := make(map[string]bool, len(images))
	visited := make(map[string]bool, len(images))
	var visit func(string) error
	visit = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("image build graph contains a cycle at %q", key)
		}
		if visited[key] {
			return nil
		}
		visiting[key] = true
		for _, step := range build.Images[images[key]].Steps {
			if step.CopyFromImage == nil {
				continue
			}
			target := step.CopyFromImage.ImageKey
			if _, ok := images[target]; !ok {
				return fmt.Errorf("image %q references unknown image %q", key, target)
			}
			if err := visit(target); err != nil {
				return err
			}
		}
		delete(visiting, key)
		visited[key] = true
		return nil
	}
	if err := visit(build.Root); err != nil {
		return err
	}
	if len(visited) != len(images) {
		for _, image := range build.Images {
			if !visited[image.Key] {
				return fmt.Errorf("image %q is unreachable from root %q", image.Key, build.Root)
			}
		}
	}
	return nil
}

func Canonical(build Build, architecture string) ([]byte, error) {
	if err := Validate(build, architecture); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(build)
	if err != nil {
		return nil, fmt.Errorf("encode image build: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize image build: %w", err)
	}
	if len(canonical) > maxImageBuildBytes {
		return nil, fmt.Errorf("image build exceeds %d bytes", maxImageBuildBytes)
	}
	return canonical, nil
}

func Digest(build Build, architecture string) (string, error) {
	canonical, err := Canonical(build, architecture)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func RegistryCredentials(build Build, architecture string) ([]RegistryCredential, error) {
	if err := Validate(build, architecture); err != nil {
		return nil, err
	}
	byAuthority := make(map[string]RegistryCredential)
	for _, image := range build.Images {
		for _, step := range image.Steps {
			if step.From == nil || step.From.Auth == nil {
				continue
			}
			authority, err := RegistryAuthority(step.From.Ref)
			if err != nil {
				return nil, err
			}
			byAuthority[authority] = RegistryCredential{
				Authority:      authority,
				Username:       step.From.Auth.Username,
				PasswordSecret: step.From.Auth.PasswordSecret,
			}
		}
	}
	credentials := make([]RegistryCredential, 0, len(byAuthority))
	for _, credential := range byAuthority {
		credentials = append(credentials, credential)
	}
	slices.SortFunc(credentials, func(left, right RegistryCredential) int {
		return strings.Compare(left.Authority, right.Authority)
	})
	return credentials, nil
}

func RegistryAuthority(value string) (string, error) {
	if err := validateImageReference(value, "image reference"); err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}
	authority, err := normalizeRegistryAuthority(reference.Domain(named))
	if err != nil {
		return "", err
	}
	switch authority {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "docker.io", nil
	default:
		return authority, nil
	}
}

func normalizeRegistryAuthority(value string) (string, error) {
	authority := strings.ToLower(value)
	if !strings.Contains(authority, ":") {
		return authority, nil
	}
	host, portValue, err := net.SplitHostPort(authority)
	if err != nil {
		if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
			if net.ParseIP(strings.Trim(authority, "[]")) == nil {
				return "", fmt.Errorf("registry authority %q is invalid", value)
			}
			return authority, nil
		}
		return "", fmt.Errorf("registry authority %q is invalid: %w", value, err)
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("registry authority %q has an invalid port", value)
	}
	if port == 443 {
		if strings.Contains(host, ":") {
			return "[" + host + "]", nil
		}
		return host, nil
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func StepCount(build Build) int {
	total := 0
	for _, image := range build.Images {
		total += len(image.Steps)
	}
	return total
}

func validateStep(step Step, imageKey string, index int) (int, error) {
	count := 0
	for _, present := range []bool{
		step.From != nil,
		step.Run != nil,
		step.CopySourceFile != nil,
		step.CopySourceDir != nil,
		step.CopyFromImage != nil,
		step.Workdir != nil,
		step.User != nil,
		step.Env != nil,
	} {
		if present {
			count++
		}
	}
	if count != 1 {
		return 0, fmt.Errorf("image %q step %d must contain exactly one operation", imageKey, index)
	}
	label := fmt.Sprintf("image %q step %d", imageKey, index)
	switch {
	case step.From != nil:
		if err := validateImageReference(step.From.Ref, label+" from.ref"); err != nil {
			return 0, err
		}
	case step.Run != nil:
		if err := validateRun(*step.Run, label+" run"); err != nil {
			return 0, err
		}
	case step.CopySourceFile != nil:
		value := step.CopySourceFile
		if err := validateImageAbsolutePath(value.Dst, label+" copySourceFile.dst"); err != nil {
			return 0, err
		}
		if err := validateImageSourcePath(value.Path, false, label+" copySourceFile.path"); err != nil {
			return 0, err
		}
	case step.CopySourceDir != nil:
		value := step.CopySourceDir
		if err := validateImageAbsolutePath(value.Dst, label+" copySourceDir.dst"); err != nil {
			return 0, err
		}
		if err := validateImageSourcePath(value.Path, true, label+" copySourceDir.path"); err != nil {
			return 0, err
		}
	case step.CopyFromImage != nil:
		value := step.CopyFromImage
		if err := validateImageAbsolutePath(value.Dst, label+" copyFromImage.dst"); err != nil {
			return 0, err
		}
		if err := validateImageIdentifier(value.ImageKey, label+" copyFromImage.imageKey"); err != nil {
			return 0, err
		}
		if err := validateImageAbsolutePath(value.SrcPath, label+" copyFromImage.srcPath"); err != nil {
			return 0, err
		}
	case step.Workdir != nil:
		if err := validateWorkdir(step.Workdir.Path, label+" workdir.path"); err != nil {
			return 0, err
		}
	case step.User != nil:
		name := step.User.Name
		if !validImageString(name, maxUserBytes) ||
			strings.TrimSpace(name) != name ||
			!imageUserPattern.MatchString(name) {
			return 0, fmt.Errorf("%s user.name is not a valid OCI user or user:group", label)
		}
	case step.Env != nil:
		if !imageEnvKeyPattern.MatchString(step.Env.Key) ||
			len(step.Env.Key) > maxEnvKeyBytes {
			return 0, fmt.Errorf("%s env.key is invalid", label)
		}
		if !validImageString(step.Env.Value, maxEnvValueBytes) {
			return 0, fmt.Errorf("%s env.value is invalid", label)
		}
		return len(step.Env.Key) + len(step.Env.Value), nil
	}
	return 0, nil
}

func validateRun(run Run, label string) error {
	if run.Argv == nil {
		return fmt.Errorf("%s argv must be an array", label)
	}
	if len(run.Argv) == 0 || len(run.Argv) > maxImageArguments {
		return fmt.Errorf("%s argv count is outside [1,%d]", label, maxImageArguments)
	}
	argumentBytes := 0
	for index, argument := range run.Argv {
		if !validImageString(argument, maxImageArgumentBytes) {
			return fmt.Errorf("%s argv[%d] is invalid", label, index)
		}
		if index == 0 && argument == "" {
			return fmt.Errorf("%s argv[0] must be non-empty", label)
		}
		argumentBytes += len(argument)
	}
	if argumentBytes > maxImageArgumentsBytes {
		return fmt.Errorf("%s argv exceeds %d bytes", label, maxImageArgumentsBytes)
	}
	return nil
}

func validateImageReference(value string, label string) error {
	if !validImageString(value, maxImageReferenceBytes) ||
		strings.TrimSpace(value) != value ||
		value == "" {
		return fmt.Errorf("%s is invalid", label)
	}
	if _, err := reference.ParseNormalizedNamed(value); err != nil {
		return fmt.Errorf("%s is not a Docker-compatible OCI reference: %w", label, err)
	}
	return nil
}

func validateRegistryBinding(
	from From,
	label string,
	credentials map[string]RegistryCredential,
) error {
	if from.Auth == nil {
		return nil
	}
	if !validImageString(from.Auth.Username, maxRegistryUsernameBytes) ||
		from.Auth.Username == "" ||
		strings.TrimSpace(from.Auth.Username) != from.Auth.Username {
		return fmt.Errorf("%s from.auth.username is invalid", label)
	}
	for _, character := range from.Auth.Username {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s from.auth.username contains a control character", label)
		}
	}
	if !secretNamePattern.MatchString(from.Auth.PasswordSecret) {
		return fmt.Errorf("%s from.auth.passwordSecret is invalid", label)
	}
	authority, err := RegistryAuthority(from.Ref)
	if err != nil {
		return fmt.Errorf("%s from auth authority: %w", label, err)
	}
	binding := RegistryCredential{
		Authority:      authority,
		Username:       from.Auth.Username,
		PasswordSecret: from.Auth.PasswordSecret,
	}
	existing, ok := credentials[authority]
	if ok {
		if existing.Username != binding.Username ||
			existing.PasswordSecret != binding.PasswordSecret {
			return fmt.Errorf("%s conflicts with the existing authentication for registry %q", label, authority)
		}
		return nil
	}
	if len(credentials) == maxRegistryAuthorities {
		return fmt.Errorf("image build has more than %d authenticated registry authorities", maxRegistryAuthorities)
	}
	credentials[authority] = binding
	return nil
}

func validateImageAbsolutePath(value string, label string) error {
	if !validImageString(value, maxImagePathBytes) ||
		!path.IsAbs(value) ||
		path.Clean(value) != value ||
		hasParentPathComponent(value) {
		return fmt.Errorf("%s must be a clean absolute POSIX path", label)
	}
	return nil
}

func validateWorkdir(value string, label string) error {
	if !validImageString(value, maxImagePathBytes) ||
		value == "" ||
		path.Clean(value) != value ||
		hasParentPathComponent(value) {
		return fmt.Errorf("%s must be a clean POSIX path", label)
	}
	return nil
}

func validateImageSourcePath(value string, allowDot bool, label string) error {
	if !validImageString(value, maxImagePathBytes) ||
		value == "" ||
		path.IsAbs(value) ||
		path.Clean(value) != value ||
		hasParentPathComponent(value) ||
		(value == "." && !allowDot) {
		return fmt.Errorf("%s must be a clean Deployment-relative POSIX path", label)
	}
	return nil
}

func hasParentPathComponent(value string) bool {
	for component := range strings.SplitSeq(value, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func validateImageIdentifier(value string, label string) error {
	if value == "" ||
		!validImageString(value, maxImageIdentifierBytes) ||
		strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains a control character", label)
		}
	}
	return nil
}

func validImageString(value string, maxBytes int) bool {
	return utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') &&
		len(value) <= maxBytes
}

func validImageArchitecture(value string) bool {
	return value == "x86_64"
}
