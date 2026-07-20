package deployment

import (
	"bytes"
	"testing"
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
		parsed.Declarations[0].ModulePath != "src/build.ts" ||
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
		"platform module": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ModulePath = "helmr/task.js"
			return locator
		},
		"declaration file": func(locator DeclarationLocator) DeclarationLocator {
			locator.Declarations[0].ModulePath = "src/task.d.ts"
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
	for name, raw := range map[string][]byte{
		"unknown": []byte(
			`{"declarations":[{"declaredId":"build","exportName":"build","kind":"task","modulePath":"build.js","unknown":true}],"formatVersion":0}`,
		),
		"noncanonical": []byte(
			`{"formatVersion":0,"declarations":[{"declaredId":"build","exportName":"build","kind":"task","modulePath":"build.js"}]}`,
		),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDeclarationLocator(raw); err == nil {
				t.Fatal("ParseDeclarationLocator returned nil error")
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
				ModulePath: "src/build.ts",
				ExportName: "build",
			},
			{
				Kind:       DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: "src/chat.mjs",
				ExportName: "対話",
			},
		},
	}
}
