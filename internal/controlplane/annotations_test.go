package controlplane

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeTokenAnnotationsTreatsTagsAsASet(t *testing.T) {
	metadata, tags, err := normalizeTokenAnnotations(
		json.RawMessage(`{"b":2,"a":1}`),
		[]string{" beta ", "alpha", "beta"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(metadata) != `{"a":1,"b":2}` {
		t.Fatalf("metadata = %s", metadata)
	}
	if !slices.Equal(tags, []string{"alpha", "beta"}) {
		t.Fatalf("tags = %#v", tags)
	}
}

func TestNormalizeAnnotationsAppliesOwnerCardinalityAfterDeduplication(t *testing.T) {
	raw := []string{"same", " same "}
	for i := range maxTokenTags - 1 {
		raw = append(raw, string(rune('a'+i)))
	}
	if _, _, err := normalizeTokenAnnotations(nil, raw); err != nil {
		t.Fatalf("deduplicated Token tags failed: %v", err)
	}
	raw = append(raw, "extra")
	if _, _, err := normalizeTokenAnnotations(nil, raw); err == nil {
		t.Fatal("Token annotations accepted more than ten unique tags")
	}

	waitTags := make([]string, maxWaitTags)
	for i := range waitTags {
		waitTags[i] = string(rune(0x100 + i))
	}
	if _, _, err := normalizeWaitAnnotations(nil, waitTags); err != nil {
		t.Fatalf("Wait tags failed: %v", err)
	}
	waitTags = append(waitTags, "overflow")
	if _, _, err := normalizeWaitAnnotations(nil, waitTags); err == nil {
		t.Fatal("Wait annotations accepted more than thirty-two unique tags")
	}
}

func TestNormalizeAnnotationsRejectsInvalidTagAndMetadata(t *testing.T) {
	if _, _, err := normalizeTokenAnnotations(json.RawMessage(`[]`), nil); err == nil {
		t.Fatal("annotations accepted non-object metadata")
	}
	if _, _, err := normalizeTokenAnnotations(nil, []string{" \t "}); err == nil {
		t.Fatal("annotations accepted an empty normalized tag")
	}
	if _, _, err := normalizeTokenAnnotations(nil, []string{string(make([]byte, maxTagBytes+1))}); err == nil {
		t.Fatal("annotations accepted an oversized tag")
	}
	expanded := json.RawMessage(`{"values":[` + strings.Repeat(`1e20,`, 3_000) + `1e20]}`)
	if _, _, err := normalizeTokenAnnotations(expanded, nil); err == nil {
		t.Fatal("annotations accepted metadata whose normalized form exceeds 64 KiB")
	}
	rawWhitespace := json.RawMessage(`{` + strings.Repeat(" ", 70<<10) + `"ok":true}`)
	normalized, _, err := normalizeTokenAnnotations(rawWhitespace, nil)
	if err != nil {
		t.Fatalf("annotations rejected a small normalized object: %v", err)
	}
	if string(normalized) != `{"ok":true}` {
		t.Fatalf("normalized metadata = %s", normalized)
	}
}
