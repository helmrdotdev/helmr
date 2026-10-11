package verify

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestProgramArtifactAcceptsProgram(t *testing.T) {
	program := newTestProgram(t)
	verified, err := verifyProgramArtifact(context.Background(), program.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Metadata().Definitions[0].DeclaredID != "build" {
		t.Fatalf("verified index = %#v", verified.Metadata())
	}
}

func TestProgramArtifactRejectsContractDivergence(t *testing.T) {
	tests := map[string]func(*testProgram){
		"Program index": func(program *testProgram) {
			program.artifact.Files["helmr/program-metadata.json"] = []byte(
				`{"declarations":[],"formatVersion":0}`,
			)
		},
		"program entry": func(program *testProgram) {
			program.artifact.AddFile("helmr/entry.mjs", []byte("process.exit(0)\n"), 0644)
		},
		"reserved receipt path": func(program *testProgram) {
			program.artifact.AddFile("helmr/receipt.json", []byte("{}"), 0o644)
		},
		"unknown Platform-owned path": func(program *testProgram) {
			program.artifact.AddFile("helmr/modules.json", []byte("{}"), 0o644)
		},
		"evaluated config": func(program *testProgram) {
			program.artifact.Files["helmr/config.json"] = []byte("{}")
		},
		"source bytes": func(program *testProgram) {
			program.artifact.ReplaceFile("helmr/app/entry-0.mjs", []byte("export default null"))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			mutate(program)
			if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err == nil {
				t.Fatal("verifyProgramArtifact returned nil error")
			}
		})
	}
}

func TestProgramArtifactDoesNotInterpretProducerMetadata(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.Files["bun.lock"] = []byte("changed by lifecycle")
	program.artifact.Files["package.json"] = []byte(
		`{"packageManager":"yarn@4.9.2"}`,
	)

	program.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatalf("verifyProgramArtifact rejected producer metadata: %v", err)
	}
}

func TestProgramArtifactAcceptsManagerNativeDependencyTree(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.AddDirectory("node_modules/tool")
	program.artifact.AddFile("node_modules/tool/package.json", []byte(`{"name":"tool"}`), 0644)
	program.artifact.AddDirectory("packages")
	program.artifact.AddDirectory("packages/local")
	program.artifact.AddDirectory("packages/local/node_modules")
	program.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactAcceptsLocalPackageInstallLayouts(t *testing.T) {
	for _, copied := range []bool{false, true} {
		name := "symlinked"
		if copied {
			name = "copied"
		}
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			program.artifact.AddDirectory("packages")
			program.artifact.AddDirectory("packages/local")
			program.artifact.AddFile(
				"packages/local/package.json",
				[]byte(`{"name":"@example/local"}`),
				0644,
			)
			program.artifact.AddDirectory("node_modules/@example")
			if copied {
				program.artifact.AddDirectory("node_modules/@example/local")
				program.artifact.AddFile(
					"node_modules/@example/local/package.json",
					[]byte(`{"name":"@example/local"}`),
					0644,
				)
			} else {
				program.artifact.AddLink(
					"node_modules/@example/local",
					"../../packages/local",
				)
			}
			program.refreshManifest(t)
			if _, err := verifyProgramArtifact(
				context.Background(),
				program.descriptor,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramArtifactBindsExternalDependencyResolution(t *testing.T) {
	newExternalProgram := func(t *testing.T, target string) *testProgram {
		t.Helper()
		program := newTestProgram(t)
		program.artifact.AddDirectory("node_modules/.pnpm")
		program.artifact.AddDirectory("node_modules/.pnpm/registry-package")
		program.artifact.AddFile(
			"node_modules/.pnpm/registry-package/index.mjs",
			[]byte("export const value = true\n"),
			0644,
		)
		program.artifact.AddLink("node_modules/registry-package", target)
		program.refreshManifest(t)
		return program
	}

	valid := newExternalProgram(t, ".pnpm/registry-package")
	if _, err := verifyProgramArtifact(context.Background(), valid.descriptor); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*testProgram){
		"missing": func(program *testProgram) {
			delete(program.artifact.Files, "node_modules/.pnpm/registry-package/index.mjs")
		},
		"broken": func(program *testProgram) {
			program.artifact.Mutate("node_modules/registry-package", func(entry *artifact.Entry) {
				entry.LinkTarget = ".pnpm/missing"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
		"misdirected": func(program *testProgram) {
			program.artifact.AddDirectory("node_modules/.pnpm/other")
			program.artifact.AddFile(
				"node_modules/.pnpm/other/index.mjs",
				[]byte("export const value = false\n"),
				0644,
			)
			program.artifact.Mutate("node_modules/registry-package", func(entry *artifact.Entry) {
				entry.LinkTarget = ".pnpm/other"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
		"escaping": func(program *testProgram) {
			program.artifact.Mutate("node_modules/registry-package", func(entry *artifact.Entry) {
				entry.LinkTarget = "../../outside"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			program := newExternalProgram(t, ".pnpm/registry-package")
			mutate(program)
			if _, err := verifyProgramArtifact(
				context.Background(),
				program.descriptor,
			); err == nil {
				t.Fatal("verifyProgramArtifact returned nil error")
			}
		})
	}
}

func TestProgramArtifactValidatesNamespaceLinks(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.AddDirectory("node_modules/.bin")
	program.artifact.AddDirectory("node_modules/tool")
	program.artifact.AddFile("node_modules/tool/index.js", []byte("export {}\n"), 0644)
	program.artifact.AddLink("node_modules/.bin/tool", "../tool/index.js")
	program.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}

	escaping := newTestProgram(t)
	escaping.artifact.AddLink("node_modules/escape", "../../outside")
	if _, err := verifyProgramArtifact(context.Background(), escaping.descriptor); err == nil {
		t.Fatal("verifyProgramArtifact accepted an escaping dependency link")
	}

	dangling := newTestProgram(t)
	dangling.artifact.AddFile("file", []byte("x"), 0644)
	dangling.artifact.AddLink("safe", "file/../..")
	dangling.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), dangling.descriptor); err != nil {
		t.Fatalf("verifyProgramArtifact rejected a confined ENOTDIR link: %v", err)
	}
}

func TestProgramArtifactAcceptsUnrelatedTypeScriptWithoutSidecars(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.AddFile("source.ts", []byte("export const value = 1\n"), 0644)
	program.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

type testProgram struct {
	descriptor artifactInput
	artifact   *artifacttest.Memory
	manifest   artifact.ProgramManifest
}

func newTestProgram(t *testing.T) *testProgram {
	t.Helper()
	lockfile := []byte("lockfileVersion = 1\n")
	configSourceRaw := []byte(
		`import { defineConfig } from "@helmr/sdk"; export default defineConfig({ dirs: ["tasks"] });`,
	)
	configRaw := []byte(
		`{"assets":[],"dirs":["tasks"],"external":[],"ignorePatterns":[]}`,
	)
	sourcePath := "helmr/app/entry-0.mjs"
	sourceRaw := []byte("export const build = {}\n")
	index := artifacttest.ProgramMetadata(t)
	index.ConfigResultDigest = artifacttest.Digest(string(configRaw))
	programRaw, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	manifest := artifact.ProgramManifest{
		FormatVersion: artifact.ProgramManifestFormatVersion,
		Config: artifact.ProgramPathDigest{
			Digest: artifacttest.Digest(string(configRaw)),
			Path:   "helmr/config.json",
		},
		PayloadDigest:         artifacttest.Digest("pending"),
		ProgramMetadataDigest: artifacttest.Digest(string(programRaw)),
	}
	manifestRaw, err := artifact.CanonicalProgramManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	memory := artifacttest.NewMemory()
	memory.AddDirectory("helmr")
	memory.AddDirectory("node_modules")
	memory.AddDirectory("helmr/app")
	memory.AddFile("helmr/program-manifest.json", manifestRaw, 0644)
	memory.AddFile("helmr/config.json", configRaw, 0644)
	memory.AddFile("helmr/program-metadata.json", programRaw, 0644)
	agentRaw, err := artifact.CanonicalDefinitionIndex(artifacttest.DefinitionIndex())
	if err != nil {
		t.Fatal(err)
	}
	memory.AddFile("helmr/definition-index.json", agentRaw, 0644)
	memory.AddFile(artifacttest.ModulePath("b"), []byte("export const chat = {}"), 0644)
	memory.AddFile(sourcePath, sourceRaw, 0644)
	memory.AddFile("helmr.config.ts", configSourceRaw, 0644)
	memory.AddFile("package.json", []byte(`{"packageManager":"bun@1.3.13"}`), 0644)
	memory.AddFile("bun.lock", lockfile, 0644)

	program := &testProgram{
		descriptor: artifactInput{
			Digest:    artifacttest.Digest("Program Artifact"),
			SizeBytes: artifact.SquashFSPhysicalAlign,
			MediaType: artifact.ProgramArtifactMediaType,
			Reader:    memory,
		},
		artifact: memory,
		manifest: manifest,
	}
	program.refreshManifest(t)
	return program
}

func (program *testProgram) refreshManifest(t *testing.T) {
	t.Helper()
	for name, body := range program.artifact.Files {
		program.artifact.Mutate(name, func(entry *artifact.Entry) { entry.SizeBytes = int64(len(body)) })
	}
	entries, err := program.artifact.Entries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := artifact.PayloadDigest(t.Context(), entries, program.artifact.Open)
	if err != nil {
		t.Fatal(err)
	}
	program.manifest.PayloadDigest = digest
	raw, err := artifact.CanonicalProgramManifest(program.manifest)
	if err != nil {
		t.Fatal(err)
	}
	program.artifact.Files["helmr/program-manifest.json"] = raw
	program.artifact.Mutate("helmr/program-manifest.json", func(entry *artifact.Entry) {
		entry.SizeBytes = int64(len(raw))
	})
}

func digestBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return sha256sum.FormatDigest(digest[:])
}

func TestProgramArtifactRequiresExactDefinitionIndex(t *testing.T) {
	for name, mutate := range map[string]func(*testProgram){
		"missing index": func(p *testProgram) { delete(p.artifact.Files, "helmr/definition-index.json") },
		"old path": func(p *testProgram) {
			raw := p.artifact.Files["helmr/definition-index.json"]
			delete(p.artifact.Files, "helmr/definition-index.json")
			p.artifact.AddFile("helmr/agents.json", raw, 0644)
		},
		"invalid index": func(p *testProgram) { p.artifact.ReplaceFile("helmr/definition-index.json", []byte(`{}`)) },
		"Agent identity mismatch": func(p *testProgram) {
			index := artifacttest.DefinitionIndex()
			index.Agents[0].ID = "other"
			raw, err := artifact.CanonicalDefinitionIndex(index)
			if err != nil {
				t.Fatal(err)
			}
			p.artifact.ReplaceFile("helmr/definition-index.json", raw)
		},
		"Computer module missing": func(p *testProgram) {
			index := artifacttest.DefinitionIndex()
			index.Computers[0].ThroughAgent = false
			index.Computers[0].ModulePath = "helmr/app/entry-9.mjs"
			raw, err := artifact.CanonicalDefinitionIndex(index)
			if err != nil {
				t.Fatal(err)
			}
			p.artifact.ReplaceFile("helmr/definition-index.json", raw)
		},
		"index symlink": func(p *testProgram) {
			p.artifact.Mutate("helmr/definition-index.json", func(e *artifact.Entry) {
				e.Kind = artifact.EntrySymlink
				e.LinkTarget = "program-metadata.json"
				e.SizeBytes = 0
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newTestProgram(t)
			mutate(p)
			if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
				t.Fatal("accepted invalid definition index")
			}
		})
	}
}
