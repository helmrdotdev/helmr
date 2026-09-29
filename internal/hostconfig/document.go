// Package hostconfig evaluates helmr.config.ts once on the invoking host and
// turns it into the plain document every later build phase consumes. The
// target never imports the config again.
package hostconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

// Document is the resolved config. It is internal to one build: it is neither
// deployed nor versioned, and it never contains secret values.
type Document struct {
	Discovery deployment.BuildConfig `json:"discovery"`
	Build     Build                  `json:"build"`
}

type Build struct {
	Builder        Builder  `json:"builder"`
	InstallCommand string   `json:"installCommand,omitempty"`
	Secrets        []string `json:"secrets"`
}

type Builder struct {
	Steps []Step `json:"steps"`
}

// Step is one ordered preparation step. Kind selects which fields are set.
type Step struct {
	Kind        string   `json:"kind"`
	Argv        []string `json:"argv,omitempty"`
	Source      string   `json:"source,omitempty"`
	Destination string   `json:"destination,omitempty"`
}

// reservedDestinations are trees Helmr owns inside the build environment. The
// check is a diagnostic for copy destinations; run steps are not inspected.
var reservedDestinations = []string{"/opt/helmr", "/nix", "/workspace"}

func parseDocument(raw []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode resolved config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("resolved config contains trailing data")
	}
	if document.Build.Builder.Steps == nil || document.Build.Secrets == nil {
		return Document{}, errors.New("resolved config is incomplete")
	}
	return document, nil
}

// Resolve validates the document against the captured project source, the only
// tree build environment COPY steps may read.
func (document *Document) Resolve(captured string) error {
	if _, err := deployment.CanonicalBuildConfig(document.Discovery); err != nil {
		return err
	}
	steps := document.Build.Builder.Steps
	root, err := os.OpenRoot(captured)
	if err != nil {
		return fmt.Errorf("open captured source: %w", err)
	}
	defer root.Close()
	for index := range steps {
		step := &steps[index]
		label := fmt.Sprintf("build.builder step %d", index+1)
		switch step.Kind {
		case "run":
			if step.Source != "" || step.Destination != "" {
				return fmt.Errorf("%s run has copy fields", label)
			}
			if err := imagebuild.ValidateRunArgv(step.Argv, label+" run"); err != nil {
				return err
			}
			for _, argument := range step.Argv {
				if argument == "" || strings.ContainsAny(argument, "\r\n") {
					return fmt.Errorf("%s run arguments must be non-empty single lines; copy a script for multi-line setup", label)
				}
			}
		case "copy":
			if step.Argv != nil {
				return fmt.Errorf("%s copy has run fields", label)
			}
			if err := imagebuild.ValidateAbsolutePath(step.Destination, label+" copy destination"); err != nil {
				return err
			}
			if strings.Contains(step.Destination, `\`) {
				return fmt.Errorf("%s copy destination %q contains '\\', which Docker treats as an escape; use a literal POSIX path", label, step.Destination)
			}
			for _, reserved := range reservedDestinations {
				if step.Destination == reserved || strings.HasPrefix(step.Destination, reserved+"/") {
					return fmt.Errorf("%s copy destination %s is inside %s, which Helmr owns; use a location such as /usr/local or /opt/<name>", label, step.Destination, reserved)
				}
			}
			if err := capturedSource(root, step.Source, label); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s has unknown kind %q", label, step.Kind)
		}
	}
	return nil
}

// capturedSource admits a literal path: clean, relative, every component a
// real directory, and the target a regular file or directory in the capture.
// Docker reads COPY sources as patterns. Only "[" has a proven literal
// encoding (see builder.literalCopySource); "*", "?" and "\\" have none, so
// names containing them are refused instead of silently selecting other files.
func capturedSource(root *os.Root, source, label string) error {
	if source == "" || len(source) > 4096 || strings.ContainsAny(source, "\x00\r\n") ||
		path.IsAbs(source) || path.Clean(source) != source || source == ".." || strings.HasPrefix(source, "../") {
		return fmt.Errorf("%s copy source must be a clean path relative to the project root", label)
	}
	if strings.ContainsAny(source, `*?\`) {
		return fmt.Errorf("%s copy source %q contains '*', '?' or '\\'; sources are literal paths, so copy the containing directory instead", label, source)
	}
	if source == "." {
		return nil
	}
	components := strings.Split(source, "/")
	for index := range components {
		info, err := root.Lstat(path.Join(components[:index+1]...))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s copy source %q is not in the captured project source; host-only files and ignored paths cannot be copied", label, source)
		}
		if err != nil {
			return fmt.Errorf("%s copy source %q: %w", label, source, err)
		}
		last := index == len(components)-1
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s copy source %q crosses a symbolic link", label, source)
		case info.IsDir(), last && info.Mode().IsRegular():
		default:
			return fmt.Errorf("%s copy source %q is not a regular file or directory", label, source)
		}
	}
	return nil
}
