package hostconfig

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sdkCopy is a stand-in for an installed @helmr/sdk. Real copies share only
// the global builder brand with the CLI's embedded inspector, which is what
// these tests exercise; variant selects a current or stale release.
func sdkCopy(variant string) map[string]string {
	index := `
const brand = Symbol.for("helmr.sdk.v0.builder")
class Builder {
  constructor(steps = []) { this.steps = Object.freeze(steps); Object.defineProperty(this, brand, { value: true }); Object.freeze(this) }
  run(argv) { return new Builder([...this.steps, Object.freeze({ kind: "run", argv: Object.freeze([...argv]) })]) }
  copy(source, destination) { return new Builder([...this.steps, Object.freeze({ kind: "copy", source, destination })]) }
  mount(target) { return new Builder([...this.steps, Object.freeze({ kind: "mount", target })]) }
}
export const builder = () => new Builder()
export const copyName = ` + "`" + variant + "`" + `
export function defineConfig(config) { return Object.freeze({ ...config }) }
`
	if variant == "stale" {
		index = `
export function defineConfig(config) {
  for (const key of Object.keys(config)) if (key !== "dirs" && key !== "ignorePatterns") throw new Error("config accepts only dirs and ignorePatterns")
  return config
}
`
	}
	return map[string]string{"package.json": `{"name":"@helmr/sdk","type":"module","exports":"./index.js"}`, "index.js": index}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func installSDK(t *testing.T, modules, variant string) {
	t.Helper()
	for name, body := range sdkCopy(variant) {
		writeTree(t, filepath.Join(modules, "@helmr", "sdk"), map[string]string{name: body})
	}
}

func evaluate(t *testing.T, project string) (Document, string, error) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("host config evaluation needs node on PATH")
	}
	resolved, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	document, err := Evaluate(t.Context(), resolved, &diagnostics)
	return document, diagnostics.String(), err
}

// A monorepo: dependencies are hoisted above the project, one helper is an
// extensionless TypeScript import, one arrives through a tsconfig path, and a
// workspace package ships TypeScript source with an enum.
func TestEvaluateUsesOrdinaryHostImportsOnce(t *testing.T) {
	workspace := t.TempDir()
	project := filepath.Join(workspace, "apps", "agent")
	installSDK(t, filepath.Join(workspace, "node_modules"), "current")
	writeTree(t, workspace, map[string]string{
		"node_modules/cjs-names/package.json": `{"name":"cjs-names","main":"index.js"}`,
		"node_modules/cjs-names/index.js":     `module.exports = { secret: "NPM_TOKEN" }`,
		"node_modules/esm-dirs/package.json":  `{"name":"esm-dirs","type":"module","exports":"./index.js"}`,
		"node_modules/esm-dirs/index.js":      `export const extra = ["checks"]`,
		"packages/setup/package.json":         `{"name":"@acme/setup","type":"module","exports":"./index.ts"}`,
		"packages/setup/index.ts":             "export enum Script { Path = \"build/setup.sh\" }\n",
		"apps/agent/package.json":             `{"name":"agent","type":"module"}`,
		"apps/agent/tsconfig.json":            `{"compilerOptions":{"baseUrl":".","paths":{"@build/*":["build/*"]}}}`,
		"apps/agent/build/dirs.ts":            "export const dirs: readonly string[] = [\"src\"]\n",
		"apps/agent/build/command.ts":         "export const command = (name: string): string => `./prepare.sh ${name}`\n",
		"apps/agent/helmr.config.ts": `
import { appendFileSync } from "node:fs"
import { builder, defineConfig } from "@helmr/sdk"
import names from "cjs-names"
import { extra } from "esm-dirs"
import { Script } from "@acme/setup"
import { dirs } from "./build/dirs"
import { command } from "@build/command"
appendFileSync(process.env.HELMR_TEST_EVALUATIONS!, "evaluated\n")
console.log("config stdout is not the result")
const prepared = builder().copy(Script.Path, "/opt/setup.sh")
export default defineConfig({
  dirs: [...dirs, ...extra],
  build: { builder: prepared.run(["/bin/sh", "/opt/setup.sh"]), installCommand: command("frozen"), secrets: [names.secret] },
})
`,
	})
	if err := os.MkdirAll(filepath.Join(workspace, "node_modules", "@acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "packages", "setup"), filepath.Join(workspace, "node_modules", "@acme", "setup")); err != nil {
		t.Fatal(err)
	}
	evaluations := filepath.Join(t.TempDir(), "evaluations")
	t.Setenv("HELMR_TEST_EVALUATIONS", evaluations)

	document, diagnostics, err := evaluate(t, project)
	if err != nil {
		t.Fatalf("%v\n%s", err, diagnostics)
	}
	want := Document{}
	want.Discovery.Dirs = []string{"checks", "src"}
	want.Discovery.IgnorePatterns = []string{}
	want.Build = Build{
		Builder: Builder{Steps: []Step{
			{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/setup.sh"},
			{Kind: "run", Argv: []string{"/bin/sh", "/opt/setup.sh"}},
		}},
		InstallCommand: "./prepare.sh frozen",
		Secrets:        []string{"NPM_TOKEN"},
	}
	if !reflect.DeepEqual(document, want) {
		t.Fatalf("document = %+v", document)
	}
	if !strings.Contains(diagnostics, "config stdout is not the result") {
		t.Fatalf("config output was not forwarded: %q", diagnostics)
	}
	if count, _ := os.ReadFile(evaluations); string(count) != "evaluated\n" {
		t.Fatalf("config ran %q, want exactly once", count)
	}
}

func TestEvaluateAddsNoHelmrEnvironmentAndInheritsTheCallers(t *testing.T) {
	project := t.TempDir()
	installSDK(t, filepath.Join(project, "node_modules"), "current")
	seen := filepath.Join(t.TempDir(), "env")
	t.Setenv("HELMR_TEST_ENV_OUT", seen)
	t.Setenv("CALLER_VISIBLE", "inherited")
	before := map[string]bool{}
	for _, entry := range os.Environ() {
		before[strings.SplitN(entry, "=", 2)[0]] = true
	}
	writeTree(t, project, map[string]string{
		"package.json":    `{"type":"module"}`,
		"helmr.config.ts": `import { writeFileSync } from "node:fs"; writeFileSync(process.env.HELMR_TEST_ENV_OUT!, Object.keys(process.env).sort().join("\n")); export default { dirs: ["src"] }`,
	})
	if _, diagnostics, err := evaluate(t, project); err != nil {
		t.Fatalf("%v\n%s", err, diagnostics)
	}
	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	inherited := false
	for _, name := range strings.Split(string(raw), "\n") {
		inherited = inherited || name == "CALLER_VISIBLE"
		// Node itself may add nothing here; anything new would be Helmr's doing.
		if !before[name] && name != "" {
			t.Fatalf("config process received %q, which the caller did not have", name)
		}
	}
	if !inherited {
		t.Fatal("caller environment was not inherited")
	}
}

func TestEvaluateExplainsHostPreparationFailures(t *testing.T) {
	for name, test := range map[string]struct {
		sdk, config string
		want        []string
	}{
		"missing package": {"current", `import "not-installed"; export default { dirs: ["src"] }`,
			[]string{"not-installed", "install the packages it imports here"}},
		"stale sdk": {"stale", `import { defineConfig } from "@helmr/sdk"; export default defineConfig({ dirs: ["src"], build: {} } as never)`,
			[]string{"accepts only dirs and ignorePatterns", "check the key's spelling and that the project's @helmr/sdk is compatible"}},
		// The same advice must be true for a current SDK and a misspelled key.
		"misspelled key": {"current", `export default { dirs: ["src"], biuld: {} }`,
			[]string{"config accepts only dirs, ignorePatterns and build", "check the key's spelling"}},
		"secret name with whitespace": {"current", `export default { dirs: ["src"], build: { secrets: [" NPM_TOKEN"] } }`,
			[]string{"environment variable names such as NPM_TOKEN"}},
		"forged builder": {"current", `export default { dirs: ["src"], build: { builder: { steps: [{ kind: "run", argv: ["true"] }] } } }`,
			[]string{"must be created by builder()"}},
		"unknown step from a newer sdk": {"current", `import { builder } from "@helmr/sdk"; export default { dirs: ["src"], build: { builder: builder().mount("/cache") } }`,
			[]string{"builder step 1", "align their versions"}},
		"unknown key": {"current", `export default { dirs: ["src"], build: { packages: ["jq"] } }`,
			[]string{"accepts only builder, installCommand and secrets"}},
		"typescript error": {"current", "export default { dirs: [\"src\" }", []string{"helmr.config.ts"}},
	} {
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			installSDK(t, filepath.Join(project, "node_modules"), test.sdk)
			writeTree(t, project, map[string]string{"package.json": `{"type":"module"}`, "helmr.config.ts": test.config})
			_, diagnostics, err := evaluate(t, project)
			if err == nil {
				t.Fatal("evaluation succeeded")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error()+diagnostics, want) {
					t.Fatalf("missing %q in:\n%v\n%s", want, err, diagnostics)
				}
			}
		})
	}
}

// Two installed SDK copies and the CLI's embedded inspector are three separate
// module instances; a builder made by one is still a builder to the others.
func TestEvaluateAcceptsABuilderAcrossSDKCopies(t *testing.T) {
	project := t.TempDir()
	installSDK(t, filepath.Join(project, "node_modules"), "current")
	installSDK(t, filepath.Join(project, "tools", "node_modules"), "nested")
	writeTree(t, project, map[string]string{
		"package.json":      `{"type":"module"}`,
		"tools/prepared.js": `import { builder, copyName } from "@helmr/sdk"; export const prepared = builder().run(["echo", copyName])`,
		"helmr.config.ts":   `import { defineConfig, copyName } from "@helmr/sdk"; import { prepared } from "./tools/prepared.js"; export default defineConfig({ dirs: [copyName === "current" ? "src" : "wrong"], build: { builder: prepared } })`,
	})
	document, diagnostics, err := evaluate(t, project)
	if err != nil {
		t.Fatalf("%v\n%s", err, diagnostics)
	}
	if !reflect.DeepEqual(document.Build.Builder.Steps, []Step{{Kind: "run", Argv: []string{"echo", "nested"}}}) ||
		!reflect.DeepEqual(document.Discovery.Dirs, []string{"src"}) {
		t.Fatalf("document = %+v", document)
	}
}

func TestEvaluateRequiresASupportedNode(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, err := Evaluate(t.Context(), t.TempDir(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "Node.js 22 or newer") {
		t.Fatalf("missing node error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\necho 20.11.1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(t.Context(), t.TempDir(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "found 20.11.1") {
		t.Fatalf("old node error = %v", err)
	}
}
