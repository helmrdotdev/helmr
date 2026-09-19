package hostconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolved(t *testing.T, captured string, steps ...Step) error {
	t.Helper()
	document := Document{Build: Build{Builder: Builder{Steps: steps}, Secrets: []string{}}}
	document.Discovery.Dirs = []string{"src"}
	document.Discovery.IgnorePatterns = []string{}
	return document.Resolve(captured)
}

func TestResolveAdmitsLiteralFilesAndDirectoriesInTheCapture(t *testing.T) {
	captured := t.TempDir()
	writeTree(t, captured, map[string]string{"build/setup.sh": "#!/bin/sh\n", "build/assets[1]/a[1].txt": "a", "build/$NAME.txt": "d"})
	if err := resolved(t, captured,
		Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/setup.sh"},
		Step{Kind: "copy", Source: "build/assets[1]", Destination: "/opt/assets"},
		Step{Kind: "copy", Source: "build/assets[1]/a[1].txt", Destination: "/opt/a.txt"},
		Step{Kind: "copy", Source: "build/$NAME.txt", Destination: "/opt/$NAME/result.txt"},
		Step{Kind: "copy", Source: ".", Destination: "/opt/project"},
		Step{Kind: "run", Argv: []string{"/bin/sh", "/opt/setup.sh"}},
	); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRejectsSourcesOutsideTheCaptureAndHelmrOwnedDestinations(t *testing.T) {
	captured := t.TempDir()
	outside := t.TempDir()
	writeTree(t, captured, map[string]string{"build/setup.sh": "x"})
	writeTree(t, outside, map[string]string{"host-only.txt": "x"})
	if err := os.Symlink(outside, filepath.Join(captured, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("setup.sh", filepath.Join(captured, "build", "alias.sh")); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		step Step
		want string
	}{
		"missing":               {Step{Kind: "copy", Source: "node_modules/pkg", Destination: "/opt/pkg"}, "not in the captured project source"},
		"parent":                {Step{Kind: "copy", Source: "../x", Destination: "/opt/x"}, "clean path relative to the project root"},
		"absolute":              {Step{Kind: "copy", Source: "/etc/passwd", Destination: "/opt/x"}, "clean path relative"},
		"through symlink":       {Step{Kind: "copy", Source: "linked/host-only.txt", Destination: "/opt/x"}, "crosses a symbolic link"},
		"symlink itself":        {Step{Kind: "copy", Source: "build/alias.sh", Destination: "/opt/x"}, "crosses a symbolic link"},
		"helmr tools":           {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/helmr/bin/bundle-builder"}, "/opt/helmr, which Helmr owns"},
		"nix":                   {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/nix"}, "/nix, which Helmr owns"},
		"workspace":             {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/workspace/project/x"}, "/workspace, which Helmr owns"},
		"relative target":       {Step{Kind: "copy", Source: "build/setup.sh", Destination: "opt/setup.sh"}, "clean absolute POSIX path"},
		"trailing slash":        {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/x/"}, "clean absolute POSIX path"},
		"empty argv":            {Step{Kind: "run", Argv: []string{}}, "argv count"},
		"multi-line argument":   {Step{Kind: "run", Argv: []string{"/bin/sh", "-c", "a\nb"}}, "copy a script"},
		"run with copy fields":  {Step{Kind: "run", Argv: []string{"true"}, Destination: "/opt/x"}, "run has copy fields"},
		"copy with run fields":  {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/x", Argv: []string{}}, "copy has run fields"},
		"escape in destination": {Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/a\\b"}, "Docker treats as an escape"},
		"star":                  {Step{Kind: "copy", Source: "build/*.sh", Destination: "/opt/x"}, "sources are literal paths"},
		"question mark":         {Step{Kind: "copy", Source: "build/setup.s?", Destination: "/opt/x"}, "sources are literal paths"},
		"backslash":             {Step{Kind: "copy", Source: "build\\setup.sh", Destination: "/opt/x"}, "sources are literal paths"},
		"unknown kind":          {Step{Kind: "from"}, `unknown kind "from"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := resolved(t, captured, Step{Kind: "run", Argv: []string{"true"}}, test.step)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "step 2") {
				t.Fatalf("error = %v, want step 2 … %q", err, test.want)
			}
		})
	}
	// A sibling that merely shares a prefix with a reserved tree is not reserved.
	if err := resolved(t, captured, Step{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/helmr-tools/setup.sh"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseDocumentIsStrict(t *testing.T) {
	valid := `{"discovery":{"dirs":["src"],"ignorePatterns":[]},"build":{"builder":{"steps":[]},"secrets":[]}}`
	if _, err := parseDocument([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown field":   `{"discovery":{"dirs":["src"],"ignorePatterns":[]},"build":{"builder":{"steps":[]},"secrets":[],"packages":[]}}`,
		"secret value":    `{"discovery":{"dirs":["src"],"ignorePatterns":[]},"build":{"builder":{"steps":[]},"secrets":[],"secretValues":{"A":"b"}}}`,
		"trailing":        valid + `{}`,
		"missing builder": `{"discovery":{"dirs":["src"],"ignorePatterns":[]},"build":{"secrets":[]}}`,
	} {
		if _, err := parseDocument([]byte(raw)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
