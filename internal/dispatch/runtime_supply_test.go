package dispatch

import (
	"strings"
	"testing"
)

func TestPreparedSupplyLockKeyIsPrintableAndInjectiveAcrossSeparators(t *testing.T) {
	left := preparedSupplyLockKey("a:b", "c", "d")
	right := preparedSupplyLockKey("a", "b:c", "d")
	if left == right {
		t.Fatal("distinct prepared supply scopes collided")
	}
	if strings.ContainsRune(left, '\x00') || strings.ContainsRune(right, '\x00') {
		t.Fatal("advisory lock key contains PostgreSQL-invalid NUL")
	}
}
