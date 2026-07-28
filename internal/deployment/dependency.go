package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var fullGitCommitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type dependencyAuthority struct {
	archives map[string]struct{}
}

func validateSourceDependencies(
	manifest map[string]any,
	manager PackageManagerName,
	lockfile selectedSourceLockfile,
) error {
	authority := dependencyAuthority{archives: make(map[string]struct{})}
	if err := authority.inspectLockfile(manager, lockfile.raw); err != nil {
		return fmt.Errorf("submitted source %s: %w", lockfile.name, err)
	}
	for _, field := range []string{
		"dependencies",
		"devDependencies",
		"optionalDependencies",
		"peerDependencies",
		"overrides",
		"resolutions",
	} {
		value, exists := manifest[field]
		if !exists {
			continue
		}
		if err := authority.inspectManifestDependencyValue(value); err != nil {
			return fmt.Errorf("submitted source package.json %s: %w", field, err)
		}
	}
	return nil
}

func (authority dependencyAuthority) inspectLockfile(
	manager PackageManagerName,
	raw []byte,
) error {
	if !utf8.Valid(raw) {
		return errors.New("lockfile is not valid UTF-8")
	}
	switch manager {
	case PackageManagerNPM:
		value, err := decodeDependencyJSON(raw, "npm lockfile")
		if err != nil {
			return err
		}
		return authority.inspectNPMValue(value)
	case PackageManagerPNPM:
		var document yaml.Node
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		if err := decoder.Decode(&document); err != nil {
			return fmt.Errorf("parse pnpm lockfile: %w", err)
		}
		var trailing yaml.Node
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return errors.New("pnpm lockfile contains multiple YAML documents")
			}
			return fmt.Errorf("finish pnpm lockfile: %w", err)
		}
		if len(document.Content) == 0 {
			return errors.New("pnpm lockfile is empty")
		}
		return authority.inspectPNPMNode(document.Content[0])
	case PackageManagerBun:
		normalized, err := normalizeJSONC(raw)
		if err != nil {
			return fmt.Errorf("parse Bun lockfile: %w", err)
		}
		value, err := decodeDependencyJSON(normalized, "Bun lockfile")
		if err != nil {
			return err
		}
		return authority.inspectBunValue(value)
	default:
		return fmt.Errorf("Manager %q is unsupported", manager)
	}
}

func decodeDependencyJSON(raw []byte, label string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder, label)
	if err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder, label); err != nil {
		return nil, err
	}
	return value, nil
}

func (authority dependencyAuthority) inspectNPMValue(value any) error {
	switch current := value.(type) {
	case map[string]any:
		if resolved, ok := current["resolved"].(string); ok {
			integrity, _ := current["integrity"].(string)
			if err := authority.inspectResolvedOrigin(resolved, integrity); err != nil {
				return err
			}
		}
		for _, key := range slices.Sorted(maps.Keys(current)) {
			if err := authority.inspectNPMValue(current[key]); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := authority.inspectNPMValue(child); err != nil {
				return err
			}
		}
	case string:
		return inspectDependencyTransport(current)
	}
	return nil
}

func (authority dependencyAuthority) inspectPNPMNode(node *yaml.Node) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := authority.inspectPNPMNode(child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		fields := make(map[string]*yaml.Node, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if key.Kind == yaml.ScalarNode && key.Tag == "!!str" {
				if _, exists := fields[key.Value]; exists {
					return fmt.Errorf("pnpm lockfile contains duplicate member %q", key.Value)
				}
				fields[key.Value] = value
			}
		}
		if tarball, exists := fields["tarball"]; exists {
			value, err := yamlString(tarball, "tarball")
			if err != nil {
				return err
			}
			integrity := ""
			if node, exists := fields["integrity"]; exists {
				integrity, err = yamlString(node, "integrity")
				if err != nil {
					return err
				}
			}
			if err := authority.inspectResolvedOrigin(value, integrity); err != nil {
				return err
			}
		}
		if repo, exists := fields["repo"]; exists {
			rawRepo, err := yamlString(repo, "repo")
			if err != nil {
				return err
			}
			commit := ""
			if node, exists := fields["commit"]; exists {
				commit, err = yamlString(node, "commit")
				if err != nil {
					return err
				}
			}
			if err := inspectGitOrigin(rawRepo, commit); err != nil {
				return err
			}
		}
		for index := 0; index < len(node.Content); index += 2 {
			if err := authority.inspectPNPMNode(node.Content[index+1]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag == "!!str" {
			return inspectDependencyTransport(node.Value)
		}
	case yaml.AliasNode:
		return errors.New("pnpm lockfile aliases are not supported")
	}
	return nil
}

func (authority dependencyAuthority) inspectBunValue(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(current)) {
			if err := authority.inspectBunValue(current[key]); err != nil {
				return err
			}
		}
	case []any:
		integrity := ""
		for _, child := range current {
			if text, ok := child.(string); ok && strings.HasPrefix(text, "sha") &&
				strings.Contains(text, "-") {
				integrity = text
			}
		}
		for _, child := range current {
			if text, ok := child.(string); ok {
				if origin, kind := dependencyOrigin(text); kind == originArchive {
					if err := authority.inspectResolvedOrigin(origin, integrity); err != nil {
						return err
					}
				} else if err := inspectDependencyTransport(text); err != nil {
					return err
				}
			} else if err := authority.inspectBunValue(child); err != nil {
				return err
			}
		}
	case string:
		return inspectDependencyTransport(current)
	}
	return nil
}

func (authority dependencyAuthority) inspectResolvedOrigin(origin, integrity string) error {
	resolved, kind := dependencyOrigin(origin)
	switch kind {
	case originNone, originLocal:
		return nil
	case originPlainHTTP:
		return errors.New("plain HTTP dependency origins are not supported")
	case originGit:
		return inspectGitSpecifier(resolved)
	case originArchive:
		if err := inspectHTTPSOrigin(resolved); err != nil {
			return err
		}
		if integrity == "" {
			return fmt.Errorf("direct archive %q is not bound by lockfile integrity", resolved)
		}
		authority.archives[resolved] = struct{}{}
	}
	return nil
}

func (authority dependencyAuthority) inspectManifestDependencyValue(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(current)) {
			if err := authority.inspectManifestDependencyValue(current[key]); err != nil {
				return err
			}
		}
	case string:
		origin, kind := dependencyOrigin(current)
		if kind == originArchive {
			if err := inspectHTTPSOrigin(origin); err != nil {
				return err
			}
			if _, exists := authority.archives[origin]; !exists {
				return fmt.Errorf("direct archive %q is not bound by lockfile integrity", origin)
			}
			return nil
		}
		return inspectDependencyTransport(current)
	case []any:
		for _, child := range current {
			if err := authority.inspectManifestDependencyValue(child); err != nil {
				return err
			}
		}
	}
	return nil
}

type dependencyOriginKind uint8

const (
	originNone dependencyOriginKind = iota
	originLocal
	originPlainHTTP
	originArchive
	originGit
)

func dependencyOrigin(value string) (string, dependencyOriginKind) {
	value = strings.TrimSpace(value)
	for _, marker := range []string{"git+https://", "git+ssh://", "git://", "ssh://"} {
		if index := strings.Index(value, marker); index >= 0 {
			return value[index:], originGit
		}
	}
	for _, marker := range []string{"github:", "gitlab:", "bitbucket:"} {
		if index := strings.Index(value, marker); index >= 0 {
			return value[index:], originGit
		}
	}
	if index := strings.Index(value, "http://"); index >= 0 {
		return value[index:], originPlainHTTP
	}
	if index := strings.Index(value, "https://"); index >= 0 {
		origin := value[index:]
		path := origin
		if parsed, err := url.Parse(origin); err == nil {
			path = parsed.Path
		}
		if strings.HasSuffix(strings.ToLower(path), ".git") {
			return origin, originGit
		}
		return origin, originArchive
	}
	for _, prefix := range []string{"file:", "link:", "workspace:", "./", "../", "/"} {
		if strings.HasPrefix(value, prefix) {
			return value, originLocal
		}
	}
	return "", originNone
}

func inspectDependencyTransport(value string) error {
	origin, kind := dependencyOrigin(value)
	switch kind {
	case originPlainHTTP:
		return errors.New("plain HTTP dependency origins are not supported")
	case originArchive:
		return inspectHTTPSOrigin(origin)
	case originGit:
		return inspectGitSpecifier(origin)
	default:
		return nil
	}
}

func inspectHTTPSOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("dependency origin %q is not valid HTTPS", origin)
	}
	if parsed.User != nil || parsed.RawQuery != "" {
		return fmt.Errorf("dependency origin %q contains credential-bearing URL authority", origin)
	}
	return validatePublicDependencyHost(parsed.Hostname())
}

func inspectGitSpecifier(specifier string) error {
	switch {
	case strings.HasPrefix(specifier, "git+https://"):
		parsed, err := url.Parse(strings.TrimPrefix(specifier, "git+"))
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
			return fmt.Errorf("git dependency %q is not credential-free HTTPS", specifier)
		}
		if err := validatePublicDependencyHost(parsed.Hostname()); err != nil {
			return fmt.Errorf("git dependency %q: %w", specifier, err)
		}
		return requireFullGitCommit(parsed.Fragment)
	case strings.HasPrefix(specifier, "https://"):
		parsed, err := url.Parse(specifier)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
			return fmt.Errorf("git dependency %q is not credential-free HTTPS", specifier)
		}
		if err := validatePublicDependencyHost(parsed.Hostname()); err != nil {
			return fmt.Errorf("git dependency %q: %w", specifier, err)
		}
		return requireFullGitCommit(parsed.Fragment)
	case strings.HasPrefix(specifier, "github:"),
		strings.HasPrefix(specifier, "gitlab:"),
		strings.HasPrefix(specifier, "bitbucket:"):
		_, commit, found := strings.Cut(specifier, "#")
		if !found {
			return fmt.Errorf("git dependency %q is not pinned to a full commit", specifier)
		}
		return requireFullGitCommit(commit)
	default:
		return fmt.Errorf("git dependency %q does not use credential-free HTTPS", specifier)
	}
}

func inspectGitOrigin(repo, commit string) error {
	if strings.HasPrefix(repo, "git+") {
		repo = strings.TrimPrefix(repo, "git+")
	}
	if err := inspectHTTPSOrigin(repo); err != nil {
		return fmt.Errorf("git repository: %w", err)
	}
	return requireFullGitCommit(commit)
}

func requireFullGitCommit(commit string) error {
	if !fullGitCommitPattern.MatchString(commit) {
		return errors.New("git dependency is not pinned to a full commit")
	}
	return nil
}

func normalizeJSONC(raw []byte) ([]byte, error) {
	withoutComments := make([]byte, 0, len(raw))
	inString := false
	escaped := false
	for index := 0; index < len(raw); index++ {
		current := raw[index]
		if inString {
			withoutComments = append(withoutComments, current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			withoutComments = append(withoutComments, current)
			continue
		}
		if current == '/' && index+1 < len(raw) {
			switch raw[index+1] {
			case '/':
				withoutComments = append(withoutComments, ' ')
				index += 2
				for index < len(raw) && raw[index] != '\n' {
					index++
				}
				withoutComments = append(withoutComments, '\n')
				continue
			case '*':
				withoutComments = append(withoutComments, ' ')
				index += 2
				closed := false
				for index < len(raw) {
					if raw[index] == '\n' {
						withoutComments = append(withoutComments, '\n')
					}
					if raw[index] == '*' && index+1 < len(raw) && raw[index+1] == '/' {
						index++
						closed = true
						break
					}
					index++
				}
				if !closed {
					return nil, errors.New("unterminated block comment")
				}
				continue
			}
		}
		withoutComments = append(withoutComments, current)
	}
	if inString {
		return nil, errors.New("unterminated string")
	}
	normalized := make([]byte, 0, len(withoutComments))
	inString = false
	escaped = false
	for index, current := range withoutComments {
		if inString {
			normalized = append(normalized, current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			normalized = append(normalized, current)
			continue
		}
		if current == ',' {
			next := index + 1
			for next < len(withoutComments) &&
				(withoutComments[next] == ' ' || withoutComments[next] == '\t' ||
					withoutComments[next] == '\r' || withoutComments[next] == '\n') {
				next++
			}
			if next < len(withoutComments) &&
				(withoutComments[next] == '}' || withoutComments[next] == ']') {
				continue
			}
		}
		normalized = append(normalized, current)
	}
	return normalized, nil
}
