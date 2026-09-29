package artifact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestDeclarationLocatorCanonicalRoundTrip(t *testing.T) {
	locator := testDeclarationLocator()
	raw, err := CanonicalDeclarationLocator(locator)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDeclarationLocator(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Declarations) != 2 ||
		parsed.Declarations[0].ModulePath != testModulePath("a") ||
		parsed.Declarations[1].ExportName != "対話" {
		t.Fatalf("parsed locator = %#v", parsed)
	}
	recoded, err := CanonicalDeclarationLocator(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, recoded) {
		t.Fatalf("canonical bytes changed:\n%s\n%s", raw, recoded)
	}
}

func TestDeclarationLocatorRejectsOpenOrDivergentShapes(t *testing.T) {
	tests := map[string]func(DeclarationLocator) DeclarationLocator{
		"empty declarations": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations = nil
			return locator
		},
		"noncanonical declaration order": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0], locator.Declarations[1] =
				locator.Declarations[1], locator.Declarations[0]
			return locator
		},
		"oversized export": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ExportName = string(bytes.Repeat([]byte("a"), 257))
			return locator
		},
		"node_modules module": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ModulePath = "node_modules/task.js"
			return locator
		},
		"noncanonical platform module": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ModulePath = "helmr/task.js"
			return locator
		},
		"build-only root config": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ModulePath = "helmr.config.ts"
			return locator
		},
		"control export": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ExportName = "bad\nname"
			return locator
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalDeclarationLocator(mutate(testDeclarationLocator())); err == nil {
				t.Fatal("CanonicalDeclarationLocator returned nil error")
			}
		})
	}
}

func TestParseDeclarationLocatorRejectsUnknownAndNoncanonicalJSON(t *testing.T) {
	valid, err := CanonicalDeclarationLocator(testDeclarationLocator())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDeclarationLocator(valid); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(valid, &object); err != nil {
		t.Fatal(err)
	}
	declarations := object["declarations"].([]any)
	declarations[0].(map[string]any)["unknown"] = true
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err = jsoncanon.Transform(unknown)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		raw  []byte
		want string
	}{
		"unknown":      {unknown, `unknown field "unknown"`},
		"noncanonical": {append([]byte(" "), valid...), "not RFC 8785 canonical JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDeclarationLocator(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseDeclarationLocator error = %v, want %q", err, test.want)
			}
		})
	}
}

func testDeclarationLocator() DeclarationLocator {
	return DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations: []LocatedDeclaration{
			{
				Kind:       DeclarationKindTask,
				DeclaredID: "build",
				ModulePath: testModulePath("a"),
				ExportName: "build",
				Slot:       DeclarationSlotHandler,
			},
			{
				Kind:       DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: testModulePath("b"),
				ExportName: "対話",
				Slot:       DeclarationSlotHandler,
			},
		},
	}
}

func testModulePath(digit string) string {
	if digit == "a" {
		return "helmr/app/entry-0.mjs"
	}
	return "helmr/app/entry-1.mjs"
}
