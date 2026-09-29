package definition

import (
	"strings"
	"testing"
)

func TestValidDeclaredID(t *testing.T) {
	for _, value := range []string{"task", "actor.v1", "sandbox_1", "image-base"} {
		if !ValidDeclaredID(value) {
			t.Fatalf("ValidDeclaredID(%q) = false", value)
		}
	}
	for _, value := range []string{"", ".task", "task/child", " task", "task ", strings.Repeat("a", 129)} {
		if ValidDeclaredID(value) {
			t.Fatalf("ValidDeclaredID(%q) = true", value)
		}
	}
}
