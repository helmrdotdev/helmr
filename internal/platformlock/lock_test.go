package platformlock

import (
	"reflect"
	"testing"
)

func TestLockKeysAreOrderIndependentAndDeduplicated(t *testing.T) {
	first := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	second := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	left, err := lockKeys([]string{second, first, second})
	if err != nil {
		t.Fatal(err)
	}
	right, err := lockKeys([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || !reflect.DeepEqual(left, right) {
		t.Fatalf("lock keys = %v and %v", left, right)
	}
}

func TestLockKeysRejectNoncanonicalDigests(t *testing.T) {
	for _, value := range []string{
		"",
		"sha256:AA",
		"1111111111111111111111111111111111111111111111111111111111111111",
		"sha256:111111111111111111111111111111111111111111111111111111111111111",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := lockKeys([]string{value}); err == nil {
				t.Fatalf("lockKeys(%q) succeeded", value)
			}
		})
	}
}
