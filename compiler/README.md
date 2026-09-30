# Program compiler

The compiler turns a captured, installed project into a selective Program payload.
The CLI evaluates `helmr.config.ts` once on the authoring host. BuildKit then runs
these stages:

1. Bundle the configured declaration directories with pinned esbuild in a scratch
   filesystem containing only the installed project and the compiler closure.
   TypeScript, `tsconfig` inheritance and static import aliases are resolved here.
   All declaration entries share one split ESM graph.
2. Install explicit runtime externals with pinned npm in a fresh copy of the
   project's build environment. Each root uses its installed exact version; npm
   resolves its runtime dependencies once. The generated lock is checked during preparation and cleaned up with temporary
   build files. A new build may resolve different transitive dependency versions.
3. Analyze only the generated payload, offline, using pinned Node and the native
   module provenance policy. Export the payload, declaration results and Computer
   Image plan together. Finalization reuses those exact bytes without evaluating
   customer modules again.

Computer Image source-copy operations still use the captured installed project.
They do not use the selective Program payload. The server executes generated
JavaScript; it does not interpret the author's TypeScript configuration.

## Runtime dependencies and files

```ts
export default defineConfig({
  build: {
    external: ["some-native-package"],
    assets: ["prompts/**", "templates/**"],
  },
});
```

`external` contains whole packages declared in the root package manifest. Use it
for native addons, packages that discover their own files or executables, and
runtime-only package lookups. Explicit roots are installed even when the bundle
has no static import of them. Keep shared peers external too when they must have
one runtime instance. The initial contract supports public-registry semver roots
and npm-compatible overrides; workspace/file/git/alias roots, private registry
installs and other package-manager override/patch dialects fail with diagnostics.
Source workspaces may still be bundled normally. Overrides retain npm's normal
semantics: a direct-root override must match the exact generated root version, or
use npm's `$packageName` reference. An author override of `"^1.0.0"` can conflict
with a generated exact root of `"1.2.3"`; npm reports that conflict during install.

`assets` contains positive project-relative file globs. Matches retain their
project-relative locations. Missing matches, symlinks and collisions with managed
metadata or dependencies fail the build. Patterns apply after source capture and
`.helmrignore`. Original source and development dependencies are not copied merely
because they existed in the project. Source maps include original source content
for debugging. Their source paths are diagnostic names from the build layout,
not files promised to exist on the Computer.

Program execution starts in `/workspace`, independently of its read-only payload.
Use ordinary package imports to locate shipped files without changing that cwd:

```json
{ "imports": { "#project/*": "./*" } }
```

```ts
const prompt = await readFile(
  new URL(import.meta.resolve("#project/prompts/system.md")),
  "utf8",
);
```

The mapping resolves a location; `build.assets` selects the files to ship. Root
package name, type, imports and exports are retained. Bundling does not preserve
arbitrary source-relative `import.meta.url` paths or dynamically computed code
entrypoints. Extra Worker entrypoints are not part of this build API.

Compiler sources are in `typescript/src`. The Linux `nix build .#compiler` package
uses the shared `platformEntries` generation output. For local tests, install the
locked workspace dependencies and run `make platform-entries` in the pinned Nix
development environment. `scripts/build-compiler-entry.sh` remains available for
focused regeneration; its output is ignored by Git. CI compares local and Nix
generation rather than checking committed bundles for freshness.
