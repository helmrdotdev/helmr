package archive

import "testing"

func TestGitIgnoreV0WildmatchVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		name    string
		match   bool
	}{
		{pattern: "foo", name: "foo", match: true},
		{pattern: "foo", name: "a/foo", match: false},
		{pattern: "foo", name: "foobar", match: false},
		{pattern: "foo*", name: "foobar", match: true},
		{pattern: "foo*", name: "foo/bar", match: false},
		{pattern: "**/foo", name: "foo", match: true},
		{pattern: "**/foo", name: "a/b/foo", match: true},
		{pattern: "**/foo/bar", name: "a/foo/bar", match: true},
		{pattern: "abc/**", name: "abc/file", match: true},
		{pattern: "abc/**", name: "abc/a/b/file", match: true},
		{pattern: "abc/**", name: "abc", match: false},
		{pattern: "a/**/b", name: "a/b", match: true},
		{pattern: "a/**/b", name: "a/x/y/b", match: true},
		{pattern: "one**a.1", name: "oneXXa.1", match: true},
		{pattern: "one**a.1", name: "one/x/a.1", match: false},
		{pattern: `foo\*`, name: "foo*", match: true},
		{pattern: `foo\*`, name: "foobar", match: false},
		{pattern: "*[al]?", name: "ball", match: true},
		{pattern: "[!]-]", name: "a", match: true},
		{pattern: "[!]-]", name: "]", match: false},
		{pattern: "[[:alpha:]][[:digit:]][[:upper:]]", name: "a1B", match: true},
		{pattern: "[[:digit:][:upper:][:space:]]", name: "A", match: true},
		{pattern: "[[:digit:][:upper:][:space:]]", name: ".", match: false},
		{pattern: "[a-c[:digit:]x-z]", name: "5", match: true},
		{pattern: "[a-c[:digit:]x-z]", name: "q", match: false},
		{pattern: "a[]b", name: "a[]b", match: false},
		{pattern: "ab[", name: "ab[", match: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.pattern+"/"+test.name, func(t *testing.T) {
			t.Parallel()
			if got := wildMatch(test.pattern, test.name); got != test.match {
				t.Fatalf("wildMatch(%q, %q) = %v, want %v", test.pattern, test.name, got, test.match)
			}
		})
	}
}

func TestGitIgnoreV0PatternRules(t *testing.T) {
	t.Parallel()
	ignore, err := parseSourceIgnore([]byte(
		"# comment\n" +
			"*.log\n" +
			"!keep.log\n" +
			"/root.txt\n" +
			"build/\n" +
			"space   \n" +
			"kept\\ \n" +
			"\\#literal\n" +
			"\\!literal\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		isDir   bool
		exclude bool
	}{
		{name: "nested/app.log", exclude: true},
		{name: "nested/keep.log", exclude: false},
		{name: "root.txt", exclude: true},
		{name: "nested/root.txt", exclude: false},
		{name: "build", isDir: true, exclude: true},
		{name: "build", exclude: false},
		{name: "space", exclude: true},
		{name: "kept ", exclude: true},
		{name: "#literal", exclude: true},
		{name: "!literal", exclude: true},
	}
	for _, test := range tests {
		if got := ignore.Match(test.name, test.isDir); got != test.exclude {
			t.Errorf("Match(%q, %v) = %v, want %v", test.name, test.isDir, got, test.exclude)
		}
	}
}
