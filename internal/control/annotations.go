package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	maxTokenTags = 10
	maxWaitTags  = 32
	maxTagBytes  = 128
)

func normalizeTokenAnnotations(metadata json.RawMessage, tags []string) (json.RawMessage, []string, error) {
	return normalizeAnnotations(metadata, tags, maxTokenTags)
}

func normalizeWaitAnnotations(metadata json.RawMessage, tags []string) (json.RawMessage, []string, error) {
	return normalizeAnnotations(metadata, tags, maxWaitTags)
}

func normalizeAnnotations(rawMetadata json.RawMessage, rawTags []string, maxTags int) (json.RawMessage, []string, error) {
	metadata := rawMetadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return nil, nil, errors.New("metadata must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &object); err != nil || object == nil {
		return nil, nil, errors.New("metadata must be a JSON object")
	}
	canonical, err := canonicalJSON(metadata)
	if err != nil {
		return nil, nil, errors.New("metadata must be a canonicalizable JSON object")
	}
	if len(canonical) > 64<<10 {
		return nil, nil, errors.New("normalized metadata must be no larger than 64 KiB")
	}

	seen := make(map[string]struct{}, len(rawTags))
	tags := make([]string, 0, min(len(rawTags), maxTags))
	for _, raw := range rawTags {
		tag := strings.TrimSpace(raw)
		if tag == "" || len(tag) > maxTagBytes {
			return nil, nil, fmt.Errorf("tags must be nonempty strings no larger than %d UTF-8 bytes", maxTagBytes)
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	if len(tags) > maxTags {
		return nil, nil, fmt.Errorf("tags must contain at most %d unique values", maxTags)
	}
	sort.Strings(tags)
	return canonical, tags, nil
}
