package safepath

import (
	"runtime"
	"strings"
	"testing"
)

func TestTreeRepresentability(t *testing.T) {
	for _, name := range []string{"日本語.txt", strings.Repeat("x", 255), strings.Repeat("dir/", 127) + "leaf", strings.Repeat("p", 80) + "/" + strings.Repeat("q", 80) + "/file"} {
		if err := ValidateTreePath(name, "/workspace/project", "/workspace/program", "/opt/helmr/program"); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "../escape", "/absolute", "a//b", "a/./b", "a\\b", "line\nfeed", string([]byte{0xff}), strings.Repeat("x", 256), strings.Repeat("dir/", 128) + "leaf"} {
		if err := ValidateTreePath(name, "/opt/helmr/program"); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	prefix := "/workspace/project"
	name := strings.Repeat(strings.Repeat("a", 254)+"/", 15)
	name += strings.Repeat("b", 4096-len(prefix)-2-len(name))
	if err := ValidateTreePath(name, prefix); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTreePath(name+"b", prefix); err == nil {
		t.Fatal("accepted mounted path overflow")
	}
}

func TestTreeLinksAndActualHostPrefix(t *testing.T) {
	for _, target := range []string{"../日本語.txt", strings.Repeat("x", 255), strings.Repeat("a/", 100) + "file"} {
		if err := ValidateTreeLink(target); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"/absolute", "a//b", strings.Repeat("x", 256), "a\x00b", strings.Repeat("a/", 2048) + "b"} {
		if err := ValidateTreeLink(target); err == nil {
			t.Fatal("accepted invalid link")
		}
	}
	limit := 4096
	if runtime.GOOS == "darwin" {
		limit = 1024
	}
	prefix := strings.Repeat("p", limit-6)
	if err := ValidateHostTreePath(prefix, "file"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHostTreePath(prefix, "files"); err == nil {
		t.Fatal("ignored actual host prefix")
	}
}
