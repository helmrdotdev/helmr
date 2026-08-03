package main

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPlatformArtifactLockKeysAreOrderIndependentAndDeduplicated(t *testing.T) {
	first := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	second := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	left, err := platformArtifactLockKeys([]string{second, first, second})
	if err != nil {
		t.Fatal(err)
	}
	right, err := platformArtifactLockKeys([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || !reflect.DeepEqual(left, right) {
		t.Fatalf("lock keys = %v and %v", left, right)
	}
}

func TestPlatformArtifactLockKeysRejectNoncanonicalDigests(t *testing.T) {
	for _, value := range []string{
		"",
		"sha256:AA",
		"1111111111111111111111111111111111111111111111111111111111111111",
		"sha256:111111111111111111111111111111111111111111111111111111111111111",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := platformArtifactLockKeys([]string{value}); err == nil {
				t.Fatalf("platformArtifactLockKeys(%q) succeeded", value)
			}
		})
	}
}

func TestPlatformArtifactLockerReleasesLockWhenFunctionPanics(t *testing.T) {
	database := dbtest.Open(t)
	locker, err := newPlatformArtifactLocker(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_ = locker.With(t.Context(), []string{digest}, func() error {
			panic("test panic")
		})
	}()
	if recovered != "test panic" {
		t.Fatalf("recovered value = %#v", recovered)
	}
	if err := locker.With(t.Context(), []string{digest}, func() error { return nil }); err != nil {
		t.Fatalf("lock remained held after panic: %v", err)
	}
}
