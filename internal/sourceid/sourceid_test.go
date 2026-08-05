package sourceid

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, value := range []string{"task", "actor.v1", "sandbox_1", "image-base"} {
		if !Valid(value) {
			t.Fatalf("Valid(%q) = false", value)
		}
	}
	for _, value := range []string{"", ".task", "task/child", " task", "task ", strings.Repeat("a", 129)} {
		if Valid(value) {
			t.Fatalf("Valid(%q) = true", value)
		}
	}
}
