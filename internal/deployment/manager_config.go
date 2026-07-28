package deployment

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

const maxSourceManagerConfigBytes = 1 << 20

func sourceManagerConfigPath(name string) bool {
	if path.Base(name) == ".npmrc" {
		return true
	}
	return name == "pnpm-workspace.yaml" || name == "bunfig.toml"
}

func validateSourceManagerConfig(name string, raw []byte) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("submitted source %s must be UTF-8", name)
	}
	var err error
	switch {
	case path.Base(name) == ".npmrc":
		err = validateNPMRC(raw)
	case name == "pnpm-workspace.yaml":
		err = validatePNPMWorkspace(raw)
	case name == "bunfig.toml":
		err = validateBunConfig(raw)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("submitted source %s: %w", name, err)
	}
	return nil
}

func validateNPMRC(raw []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), maxSourceManagerConfigBytes)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("line %d has an empty setting name", lineNumber)
		}
		if !found {
			value = "true"
		}
		value = trimManagerConfigValue(value)
		if err := validateNPMSetting(key, value); err != nil {
			return fmt.Errorf("line %d setting %q: %w", lineNumber, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	return nil
}

func validateNPMSetting(key, value string) error {
	normalized := strings.ToLower(strings.TrimSpace(key))
	leaf := normalized
	if index := strings.LastIndexByte(leaf, ':'); index >= 0 {
		leaf = leaf[index+1:]
	}
	leaf = strings.TrimSuffix(leaf, "[]")
	switch leaf {
	case "_auth", "_authtoken", "_password", "password", "username",
		"token", "tokenhelper", "token-helper":
		if value != "" {
			return errors.New("registry credentials and auth helpers are not supported")
		}
	case "cert", "certfile", "key", "keyfile":
		if value != "" {
			return errors.New("registry client credentials are not supported")
		}
	case "ca", "cafile":
		if value != "" {
			return errors.New("custom registry trust is not supported")
		}
	case "proxy", "http-proxy", "https-proxy", "noproxy", "no-proxy":
		if value != "" {
			return errors.New("registry proxy configuration is not supported")
		}
	case "strict-ssl", "strictssl":
		if value != "" && !strings.EqualFold(value, "true") {
			return errors.New("registry TLS verification cannot be weakened")
		}
	case "userconfig", "globalconfig", "npmrc-auth-file":
		if value != "" {
			return errors.New("external Manager configuration is not supported")
		}
	case "git":
		if value != "" && value != "git" {
			return errors.New("custom dependency acquisition commands are not supported")
		}
	case "registry":
		return validatePublicRegistryURL(value)
	}
	return nil
}

func validatePNPMWorkspace(raw []byte) error {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("finish YAML: %w", err)
	}
	if len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors.New("configuration must be a YAML mapping")
	}
	seen := make(map[string]struct{}, len(root.Content)/2)
	for index := 0; index < len(root.Content); index += 2 {
		keyNode := root.Content[index]
		valueNode := root.Content[index+1]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
			return errors.New("configuration setting names must be strings")
		}
		key := keyNode.Value
		if _, exists := seen[key]; exists {
			return fmt.Errorf("configuration contains duplicate setting %q", key)
		}
		seen[key] = struct{}{}
		switch key {
		case "registry":
			value, err := yamlString(valueNode, key)
			if err != nil {
				return err
			}
			if err := validatePublicRegistryURL(value); err != nil {
				return fmt.Errorf("setting %q: %w", key, err)
			}
		case "registries", "namedRegistries":
			if err := validateYAMLRegistryMap(valueNode, key); err != nil {
				return err
			}
		case "httpsProxy", "httpProxy", "noProxy":
			if !yamlNullOrEmpty(valueNode) {
				return fmt.Errorf("setting %q: registry proxy configuration is not supported", key)
			}
		case "strictSsl":
			if !yamlNullOrEmpty(valueNode) &&
				(valueNode.Tag != "!!bool" || valueNode.Value != "true") {
				return errors.New(`setting "strictSsl": registry TLS verification cannot be weakened`)
			}
		case "ca", "cafile", "cert", "certfile", "key", "keyfile":
			if !yamlNullOrEmpty(valueNode) {
				return fmt.Errorf("setting %q: custom registry trust or client credentials are not supported", key)
			}
		}
	}
	return nil
}

func validateYAMLRegistryMap(node *yaml.Node, label string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("setting %q must be a mapping", label)
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		keyNode := node.Content[index]
		valueNode := node.Content[index+1]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
			return fmt.Errorf("setting %q contains a non-string registry name", label)
		}
		if _, exists := seen[keyNode.Value]; exists {
			return fmt.Errorf("setting %q contains duplicate registry %q", label, keyNode.Value)
		}
		seen[keyNode.Value] = struct{}{}
		value, err := yamlString(valueNode, label+"."+keyNode.Value)
		if err != nil {
			return err
		}
		if err := validatePublicRegistryURL(value); err != nil {
			return fmt.Errorf("setting %q registry %q: %w", label, keyNode.Value, err)
		}
	}
	return nil
}

func yamlString(node *yaml.Node, label string) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("setting %q must be a string", label)
	}
	return node.Value, nil
}

func yamlNullOrEmpty(node *yaml.Node) bool {
	return node.Tag == "!!null" ||
		node.Kind == yaml.ScalarNode && node.Tag == "!!str" && node.Value == ""
}

func validateBunConfig(raw []byte) error {
	var root map[string]any
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("parse TOML: %w", err)
	}
	installValue, exists := root["install"]
	if !exists {
		return nil
	}
	install, ok := installValue.(map[string]any)
	if !ok {
		return errors.New(`setting "install" must be a table`)
	}
	if value, exists := install["registry"]; exists {
		if err := validateBunRegistry(value, "install.registry"); err != nil {
			return err
		}
	}
	if value, exists := install["scopes"]; exists {
		scopes, ok := value.(map[string]any)
		if !ok {
			return errors.New(`setting "install.scopes" must be a table`)
		}
		for _, scope := range slices.Sorted(maps.Keys(scopes)) {
			registry := scopes[scope]
			if err := validateBunRegistry(registry, "install.scopes."+scope); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"ca", "cafile"} {
		if value, exists := install[key]; exists && !emptyManagerValue(value) {
			return fmt.Errorf("setting %q: custom registry trust is not supported", "install."+key)
		}
	}
	if value, exists := install["security"]; exists {
		security, ok := value.(map[string]any)
		if !ok {
			return errors.New(`setting "install.security" must be a table`)
		}
		if scanner, exists := security["scanner"]; exists && !emptyManagerValue(scanner) {
			return errors.New(`setting "install.security.scanner": executable Fetch scanners are not supported`)
		}
	}
	return nil
}

func validateBunRegistry(value any, label string) error {
	switch current := value.(type) {
	case string:
		if err := validatePublicRegistryURL(current); err != nil {
			return fmt.Errorf("setting %q: %w", label, err)
		}
		return nil
	case map[string]any:
		if _, exists := current["url"]; !exists {
			return fmt.Errorf("setting %q must contain a registry URL", label)
		}
		for _, key := range slices.Sorted(maps.Keys(current)) {
			field := current[key]
			switch key {
			case "url":
				raw, ok := field.(string)
				if !ok {
					return fmt.Errorf("setting %q.url must be a string", label)
				}
				if err := validatePublicRegistryURL(raw); err != nil {
					return fmt.Errorf("setting %q.url: %w", label, err)
				}
			case "token", "username", "password":
				if !emptyManagerValue(field) {
					return fmt.Errorf("setting %q.%s: registry credentials are not supported", label, key)
				}
			default:
				return fmt.Errorf("setting %q contains unclassified security field %q", label, key)
			}
		}
		return nil
	default:
		return fmt.Errorf("setting %q must be a registry URL or table", label)
	}
}

func validatePublicRegistryURL(value string) error {
	value = trimManagerConfigValue(value)
	if value == "" {
		return errors.New("registry URL is empty")
	}
	if strings.Contains(value, "$") {
		return errors.New("registry URL interpolation is not supported")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("registry URL must be credential-free HTTPS without query or fragment")
	}
	host := parsed.Hostname()
	return validatePublicDependencyHost(host)
}

func validatePublicDependencyHost(host string) error {
	if strings.EqualFold(host, "localhost") {
		return errors.New("private dependency destinations are not supported")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() {
			return errors.New("private dependency destinations are not supported")
		}
	}
	return nil
}

func trimManagerConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first := value[0]
		last := value[len(value)-1]
		if first == last && (first == '"' || first == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func emptyManagerValue(value any) bool {
	return value == nil || value == ""
}
