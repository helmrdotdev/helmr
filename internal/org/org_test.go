package org

import (
	"errors"
	"testing"
	"uuid"
)

func TestNormalizeProjectSlugNameRejectsUUIDSlug(t *testing.T) {
	var input InputError
	if _, _, err := normalizeProjectSlugName(uuid.NewV7().String(), "Project"); !errors.As(err, &input) {
		t.Fatalf("UUID project slug error = %v, want input error", err)
	}
	if slug, name, err := normalizeProjectSlugName(" PROJECT-SLUG ", ""); err != nil || slug != "project-slug" || name != "project-slug" {
		t.Fatalf("ordinary project slug = %q/%q, %v", slug, name, err)
	}
}

func TestNormalizeEmail(t *testing.T) {
	if email, err := NormalizeEmail(" Person@Example.Test "); err != nil || email != "person@example.test" {
		t.Fatalf("email = %q, %v", email, err)
	}
	for _, value := range []string{"", "Person <person@example.test>", "not-an-address"} {
		var input InputError
		if _, err := NormalizeEmail(value); !errors.As(err, &input) {
			t.Fatalf("NormalizeEmail(%q) error = %v, want input error", value, err)
		}
	}
}
