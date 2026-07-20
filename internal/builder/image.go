package builder

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
)

const (
	ImageBuildFormatVersion = 0

	maxImageIdentifierBytes = 512
	maxImageReferenceBytes  = 4096
	maxImagePathBytes       = 4096
	maxImageArguments       = 1024
	maxImageArgumentBytes   = 65536
	maxImageArgumentsBytes  = 1 << 20
	maxImageMounts          = 256
	maxImageEnvKeyBytes     = 256
	maxImageEnvValueBytes   = 1 << 20
	maxImageEnvBytes        = 1 << 20
	maxImageUserBytes       = 256
	maxImageBuildSteps      = 10000
)

var (
	imageEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	imageSecretPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	imageUserPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?::[A-Za-z0-9_.-]+)?$`)
)

type ImageBuild struct {
	FormatVersion int         `json:"formatVersion"`
	Root          string      `json:"root"`
	Images        []ImageSpec `json:"images"`
}

type ImageSpec struct {
	Key      string        `json:"key"`
	Platform ImagePlatform `json:"platform"`
	Steps    []ImageStep   `json:"steps"`
}

type ImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type ImageStep struct {
	From           *ImageFrom           `json:"from,omitempty"`
	Run            *ImageRun            `json:"run,omitempty"`
	CopySourceFile *ImageCopySourceFile `json:"copySourceFile,omitempty"`
	CopySourceDir  *ImageCopySourceDir  `json:"copySourceDir,omitempty"`
	CopyFromImage  *ImageCopyFromImage  `json:"copyFromImage,omitempty"`
	Workdir        *ImageWorkdir        `json:"workdir,omitempty"`
	User           *ImageUser           `json:"user,omitempty"`
	Env            *ImageEnv            `json:"env,omitempty"`
}

type ImageFrom struct {
	Ref string `json:"ref"`
}

type ImageRun struct {
	Argv         []string           `json:"argv"`
	CacheMounts  []ImageCacheMount  `json:"cacheMounts"`
	SecretMounts []ImageSecretMount `json:"secretMounts"`
}

type ImageCacheMount struct {
	Dst     string `json:"dst"`
	CacheID string `json:"cacheId"`
	Sharing string `json:"sharing"`
}

type ImageSecretMount struct {
	Dst  string `json:"dst"`
	Name string `json:"name"`
}

type ImageCopySourceFile struct {
	Dst  string `json:"dst"`
	Path string `json:"path"`
}

type ImageCopySourceDir struct {
	Dst  string `json:"dst"`
	Path string `json:"path"`
}

type ImageCopyFromImage struct {
	Dst      string `json:"dst"`
	ImageKey string `json:"imageKey"`
	SrcPath  string `json:"srcPath"`
}

type ImageWorkdir struct {
	Path string `json:"path"`
}

type ImageUser struct {
	Name string `json:"name"`
}

type ImageEnv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func ValidateImageBuild(build ImageBuild, architecture string) error {
	if build.FormatVersion != ImageBuildFormatVersion {
		return fmt.Errorf(
			"image build formatVersion = %d, want %d",
			build.FormatVersion,
			ImageBuildFormatVersion,
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
		if totalSteps > maxImageBuildSteps {
			return fmt.Errorf("image build has more than %d steps", maxImageBuildSteps)
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
			addedEnvBytes, err := validateImageStep(step, image.Key, stepIndex)
			if err != nil {
				return err
			}
			envBytes += addedEnvBytes
			if envBytes > maxImageEnvBytes {
				return fmt.Errorf("image %q environment exceeds %d bytes", image.Key, maxImageEnvBytes)
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

func ImageBuildStepCount(build ImageBuild) int {
	total := 0
	for _, image := range build.Images {
		total += len(image.Steps)
	}
	return total
}

func validateImageStep(step ImageStep, imageKey string, index int) (int, error) {
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
		if err := validateImageRun(*step.Run, label+" run"); err != nil {
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
		if err := validateImageWorkdir(step.Workdir.Path, label+" workdir.path"); err != nil {
			return 0, err
		}
	case step.User != nil:
		name := step.User.Name
		if !validImageString(name, maxImageUserBytes) ||
			strings.TrimSpace(name) != name ||
			!imageUserPattern.MatchString(name) {
			return 0, fmt.Errorf("%s user.name is not a valid OCI user or user:group", label)
		}
	case step.Env != nil:
		if !imageEnvKeyPattern.MatchString(step.Env.Key) ||
			len(step.Env.Key) > maxImageEnvKeyBytes {
			return 0, fmt.Errorf("%s env.key is invalid", label)
		}
		if !validImageString(step.Env.Value, maxImageEnvValueBytes) {
			return 0, fmt.Errorf("%s env.value is invalid", label)
		}
		return len(step.Env.Key) + len(step.Env.Value), nil
	}
	return 0, nil
}

func validateImageRun(run ImageRun, label string) error {
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
	if run.CacheMounts == nil {
		return fmt.Errorf("%s cacheMounts must be an array", label)
	}
	if run.SecretMounts == nil {
		return fmt.Errorf("%s secretMounts must be an array", label)
	}
	if len(run.CacheMounts) > maxImageMounts {
		return fmt.Errorf("%s has more than %d cache mounts", label, maxImageMounts)
	}
	if len(run.SecretMounts) > maxImageMounts {
		return fmt.Errorf("%s has more than %d Secret mounts", label, maxImageMounts)
	}
	destinations := make(map[string]struct{}, len(run.CacheMounts)+len(run.SecretMounts))
	for index, mount := range run.CacheMounts {
		if err := validateImageAbsolutePath(mount.Dst, fmt.Sprintf("%s cacheMounts[%d].dst", label, index)); err != nil {
			return err
		}
		if _, ok := destinations[mount.Dst]; ok {
			return fmt.Errorf("%s has duplicate mount destination %q", label, mount.Dst)
		}
		destinations[mount.Dst] = struct{}{}
		if err := validateImageIdentifier(mount.CacheID, fmt.Sprintf("%s cacheMounts[%d].cacheId", label, index)); err != nil {
			return err
		}
		switch mount.Sharing {
		case "shared", "private", "locked":
		default:
			return fmt.Errorf("%s cacheMounts[%d].sharing is unsupported", label, index)
		}
	}
	for index, mount := range run.SecretMounts {
		if err := validateImageAbsolutePath(mount.Dst, fmt.Sprintf("%s secretMounts[%d].dst", label, index)); err != nil {
			return err
		}
		if _, ok := destinations[mount.Dst]; ok {
			return fmt.Errorf("%s has duplicate mount destination %q", label, mount.Dst)
		}
		destinations[mount.Dst] = struct{}{}
		if !imageSecretPattern.MatchString(mount.Name) {
			return fmt.Errorf("%s secretMounts[%d].name is invalid", label, index)
		}
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

func validateImageAbsolutePath(value string, label string) error {
	if !validImageString(value, maxImagePathBytes) ||
		!path.IsAbs(value) ||
		path.Clean(value) != value ||
		hasParentPathComponent(value) {
		return fmt.Errorf("%s must be a clean absolute POSIX path", label)
	}
	return nil
}

func validateImageWorkdir(value string, label string) error {
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
	return value == "aarch64" || value == "x86_64"
}
