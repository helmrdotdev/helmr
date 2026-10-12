---
title: Configuration reference
description: helmr.config.ts settings, source selection, bundling and runtime module rules.
sidebarLabel: Configuration
---

# Configuration reference

## Agent project

An Agent project contains `package.json` and `helmr.config.ts`. `.helmrignore`
is optional and selects which source files are captured; `helmr init`
generates one. See [`helmr init`](/docs/reference/cli/init).

```ts
import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["agents"],
  ignorePatterns: ["**/*.test.ts"],
})
```

## `defineConfig()`

```ts
function defineConfig(input: HelmrConfigInput): HelmrConfig

interface HelmrConfigInput {
  readonly dirs?: readonly string[]
  readonly ignorePatterns?: readonly string[]
  readonly build?: HelmrBuildInput
}

interface HelmrBuildInput {
  readonly builder?: Builder
  readonly installCommand?: string
  readonly secrets?: readonly string[]
  readonly external?: readonly string[]
  readonly assets?: readonly string[]
}
```

| Setting | Default | Description |
| --- | --- | --- |
| `dirs` | `["agents"]` | Directories, relative to the project root, searched for exported Agent and Template definitions. Must be non-empty when set, without duplicates, `..`, absolute paths or backslashes. |
| `ignorePatterns` | `[]` | Glob patterns excluded from definition discovery only. Supports `*`, `?` and whole-segment `**`; negation, brackets, braces and extended globs are rejected. |
| `build.builder` | Helmr's builder image | Steps that prepare the Linux build environment before dependencies are installed. See [Build environment](#build-environment). |
| `build.installCommand` | Inferred from the package manager | One shell command, run unprivileged with `/bin/bash -euo pipefail -c`. |
| `build.secrets` | `[]` | Names of variables in the invoking environment. Each is mounted at `/run/secrets/NAME` during dependency installation only. |
| `build.external` | `[]` | Whole public-registry package names installed beside the generated JavaScript instead of bundled. Declare each in the root `package.json`. |
| `build.assets` | `[]` | Project-relative file patterns copied into the bundle at their original locations. |

The config accepts only these settings. The default export must be an ordinary
object; `{}` is valid. Unknown settings, accessors, functions, `null` and
`undefined` fail.

## Config evaluation

The CLI evaluates `helmr.config.ts` once, on the machine that runs
`helmr build` or `helmr deploy`, before any Linux build environment exists. The
result is passed to every later step; the Linux build does not import the
config again.

- Requires Node.js 22 or newer on `PATH`. Helmr does not download Node.js.
- The config is ordinary TypeScript. Relative imports, enums, tsconfig `paths`,
  and installed ESM or CommonJS packages (including packages hoisted to a parent
  `node_modules`) resolve as usual. It is transpiled, not type-checked.
- Install what the config imports on that machine, at least `@helmr/sdk`. Helmr
  does not install host dependencies, and host `node_modules` are never copied
  into the Linux build. A native package imported by the config must be built
  for the host.
- The config is trusted local code, like a package script. It runs with your
  environment and is not sandboxed. Helmr adds no credentials to it.
- Source is captured before the config is evaluated. Build from a stable
  checkout; Helmr does not snapshot everything a config might read.
- Agent code cannot import `helmr.config.ts`, including through aliases,
  symlinks, query URLs or dynamic imports. Put shared data and helpers in
  ordinary modules.

## Package manager

The builder uses a top-level `packageManager` field, or an unambiguous npm,
pnpm, Bun or Yarn lockfile, to choose the install command. A lockfile with a
frozen install is the recommended reproducible setup; it is not required.
`build.installCommand` replaces this inference.

## Source capture

`.helmrignore` is the only source-selection file; `.gitignore` and
`.dockerignore` are not read. The root `.git` entry is always excluded. The
build fails when the captured source contains a root `node_modules` or `helmr`
path, or a `.env` or `.env.*` file other than names ending in `.example`,
`.sample` or `.template`.

`helmr build` copies the selected source into a private directory before
building. Later edits to the checkout do not change the copy. Capture rejects
changes it observes while reading, but it is not an atomic filesystem snapshot.
Files keep their executable bit; timestamps are normalized.

| Limit | Value |
| --- | --- |
| Regular-file content | 512 MiB |
| Selected entries | 100,000 |
| Path and name observation budget, including ignored children | 128 MiB |
| Path component | 255 bytes, UTF-8 |
| Directory depth | 128 |
| Symlink hops | 40 |
| Symlink target | 4,095 bytes, or the lower host limit |

Relative symlinks must stay inside the project; dangling links are allowed.
Name collisions and Unicode normalization that loses the original spelling
fail. Exclude large caches with `.helmrignore`.

Installed dependencies have separate limits:

| Limit | Value |
| --- | --- |
| Regular-file content | 10 GiB |
| Single file | 1 GiB |
| Entries, including the root | 200,000 |
| Path and link names | 128 MiB |
| Intermediate archive stream | 11 GiB |

## Build environment

Helmr's builder image is a digest-pinned Debian environment with the ELF
loader, Git, CA certificates, Python, make, a C/C++ toolchain, and Helmr's
pinned Node.js, npm, Corepack and Bun. Lifecycle scripts, development
dependencies, native compilation and dependency executables behave as on an
ordinary Linux machine. Installation runs as an unprivileged user.

```ts
import { builder, defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["agents"],
  build: {
    builder: builder()
      .copy("build/setup.sh", "/opt/setup.sh")
      .run(["/bin/sh", "/opt/setup.sh"]),
    installCommand: "./scripts/install.sh",
    secrets: ["NPM_TOKEN"],
    external: ["sharp"],
    assets: ["prompts/**"],
  },
})
```

```ts
function builder(): Builder

interface Builder {
  readonly steps: readonly BuilderStep[]
  run(argv: readonly string[]): Builder
  copy(source: string, destination: string): Builder
}
```

`builder()` takes no arguments and always starts from Helmr's builder image. It
is separate from a Computer [`image()`](/docs/reference/sdk/computer#images).

| Step | Rules |
| --- | --- |
| `copy(source, destination)` | Copies a file or directory from the captured project source (after `.helmrignore`, before installation) by project-relative path. Paths are literal: `[` and `$` mean themselves. `source` cannot contain `*`, `?` or `\`; copy the containing directory instead. `destination` cannot contain `\`. `/opt/helmr`, `/nix` and `/workspace` are reserved; use locations such as `/usr/local` or `/opt/<name>`. |
| `run(argv)` | Runs an argv array without a shell, as root, with working directory `/`, `HOME=/root`, `TMPDIR=/tmp` and `XDG_CACHE_HOME=/root/.cache`. Each step starts from that context; files persist between steps, shell state does not. Use a copied script for multi-line setup. |

The steps run once per build. The resulting environment is used for project
installation, runtime dependency installation and declaration analysis. Only
the generated JavaScript, selected assets and runtime dependencies are included
in the deployment bundle; tools installed in the build environment are not.
Computer images declare their own runtime packages.

`build.secrets` values are never written to the config result or the bundle,
and never sent to the control plane. `helmr build` fails when a named variable
is not set.

## Bundling

Helmr bundles all declaration entries into one split JavaScript module graph
with a pinned esbuild. Agent code runs that generated JavaScript on Helmr's
managed Node.js runtime; the project's TypeScript configuration is not read and
TypeScript is not transformed at runtime. Source maps include original source
content. Original source directories and build-only dependencies are not
otherwise included.

At build time, esbuild resolves imports and reads TypeScript configuration
(including package-based `extends`) within the captured, installed project. Its
TypeScript and JSX transformation rules apply; this is not type-checking. Keep
a type-check command in your own development checks.

Static imports are resolved during bundling. Computed imports and Worker
entrypoints must refer to JavaScript in the bundle or in installed runtime
packages. A dynamic import whose target is absent fails when reached.

## Runtime dependencies

Use `build.external` for native addons, packages that locate their own files
or executables, and packages looked up only at runtime. Each listed root is
installed even if no static import reaches it, at the exact version found in
the installed project. Pinned npm resolves and installs its runtime dependency
tree once in a fresh build environment; the completed tree is shipped
unchanged. A later build may select different transitive versions.

External roots must be whole package names with public-registry semver
declarations. Workspace, file, Git and alias roots, private registries, and
patch or override mechanisms other than npm `overrides` are not supported.
npm `overrides` are passed through: a direct-root override must match the
generated exact version or use npm's `$packageName` reference. Keep shared
peers external when they must have a single runtime instance.

Native code has two targets:

- Addons loaded by Agent code run inside Helmr's managed Node.js: x86_64 Linux,
  that Node.js ABI, and the runtime's own glibc and libstdc++, which are used
  instead of the Computer image's. Other shared libraries an addon links resolve
  inside the Computer: the addon's RPATH, then the image's `ld.so.cache` and
  standard library directories. The Computer image must supply them; for
  example, a project that compiles against `libvips-dev` installs `libvips42`
  in its Computer images. A missing library, or one built against a newer glibc
  than the runtime's, fails with the usual loader error when the addon is
  imported.
- Executables that Agent code spawns are ordinary Computer processes. They use
  the Computer image's loader and libraries; Helmr injects nothing into them.

One installed tree serves every Computer in a Deployment. A musl-based image can
run it when its native code is self-contained.

## Assets and file locations

`build.assets` patterns apply after source capture and `.helmrignore`. Matches
keep their project-relative locations. Missing matches, symlinks, and collisions
with managed metadata or runtime dependencies fail the build.

Agent code starts in `/workspace`; the bundle is mounted read-only elsewhere.
`/workspace` is only the default working directory. Locate shipped files with
Node.js package imports rather than the current directory:

```json
{ "imports": { "#project/*": "./*" } }
```

```ts
import { readFile } from "node:fs/promises"

const prompt = await readFile(
  new URL(import.meta.resolve("#project/prompts/system.md")),
  "utf8",
)
```

`imports` resolves locations; `build.assets` selects which files ship. The root
package's `name`, `type`, `imports` and `exports` are preserved. Paths relative
to a source file's `import.meta.url` are not preserved by bundling.

## Runtime module rules

The generated JavaScript uses native Node.js resolution. The managed entry
requires a contained, ordinary `package.json`, rejects ancestor `node_modules`
lookup, and disables global and `NODE_PATH` lookup. Modules must resolve within
the bundle or the runtime's permitted modules; TypeScript and JSX are not
compiled on demand. These rules are not a filesystem sandbox: filesystem
operations and Computer tools keep their normal access. Helmr does not inject
`NODE_OPTIONS` into other Computer processes.

## Definitions

| Surface | Fields | Reference |
| --- | --- | --- |
| `agent()` | `id`, `computer`, `turn`; optional `init`, `closeAfterIdle` (default `"7d"`, or `"never"`), `maxTurnDuration`, `triggers` | [Agent](/docs/reference/sdk/agent) |
| `template()` | `id`, `image`, `resources`; optional `prepare`, `refresh`, `deleteAfterUnused` (default `"7d"`, or `"never"`), `secrets`, `buildSecrets` | [Computer](/docs/reference/sdk/computer) |
| `triggers.cron()` | trigger ID, expression, `{ timezone, input, slack? }` | [Triggers](/docs/reference/sdk/triggers) |
| `image()` | `from`, `run`, `copy`, `copyFrom`, `workdir`, `env`, `user` | [Computer](/docs/reference/sdk/computer#images) |
| `source` | `file(path)`, `directory(path)` | [Computer](/docs/reference/sdk/computer#images) |

Computer Secret bindings reference Secrets by ID and use the same field names in
the SDK and REST API:

```json
[
  { "secretId": "SECRET_UUID", "env": { "name": "TOKEN", "mode": "raw" } },
  { "secretId": "CONFIG_SECRET_UUID", "file": { "path": "/run/secrets/config.json" } }
]
```

Create Secrets first, then use their IDs. See
[Computer](/docs/reference/sdk/computer#secret-bindings) and
[Secrets](/docs/concepts/secrets).

## Local bundle builder

`helmr build` and `helmr deploy` without `--bundle` use a dedicated Docker
Buildx builder. Its requirements, naming and lifecycle are described in
[`helmr build`](/docs/reference/cli/build#local-bundle-builder).
