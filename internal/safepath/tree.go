package safepath

import (
	"fmt"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tree bounds describe paths representable in mounted Linux build artifacts.
// Selection, reserved roots and link-chain policy belong to each tree owner.
const (
	TreeDepth          = 128
	TreeComponentBytes = 255
	TreePathBytes      = 4096
	TreeLinkBytes      = 4095
	TreeLinkHops       = 40
)

func ValidateTreePath(value string, prefixes ...string) error {
	if value == "." {
		return nil
	}
	if !treeText(value) || strings.HasPrefix(value, "/") {
		return fmt.Errorf("path is not a confined relative POSIX path")
	}
	components := strings.Split(value, "/")
	if len(components) > TreeDepth {
		return fmt.Errorf("path depth exceeds %d", TreeDepth)
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || len(component) > TreeComponentBytes {
			return fmt.Errorf("path is not normalized or exceeds a component bound")
		}
	}
	for _, prefix := range prefixes {
		if len(prefix)+1+len(value)+1 > TreePathBytes {
			return fmt.Errorf("mounted path exceeds %d bytes beneath %q", TreePathBytes, prefix)
		}
	}
	return nil
}

func ValidateTreeLink(target string) error {
	if !treeText(target) || len(target) > TreeLinkBytes || strings.HasPrefix(target, "/") {
		return fmt.Errorf("symbolic-link target is not an admitted relative POSIX path")
	}
	for component := range strings.SplitSeq(target, "/") {
		if component == "" || len(component) > TreeComponentBytes {
			return fmt.Errorf("symbolic-link target has an empty or oversized component")
		}
	}
	return nil
}

// ValidateHostTreePath includes the actual local prefix, including Darwin's
// smaller pathname limit. Filesystem-specific failures still fail explicitly.
func ValidateHostTreePath(prefix, relative string) error {
	limit := TreePathBytes
	if runtime.GOOS == "darwin" {
		limit = 1024
	}
	if len(prefix)+1+len(relative)+1 > limit {
		return fmt.Errorf("host path exceeds %d bytes beneath %q", limit, prefix)
	}
	return nil
}

func treeText(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.Contains(value, "\\") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
