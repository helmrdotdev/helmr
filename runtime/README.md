# Runtime

The TypeScript runtime executes SDK Task and Actor handlers and translates their
waits, streams, metadata and outcomes into the guest protocol. `guestd` launches
the verified platform-owned Node 24.21 runtime, fixed entry and module preload.
It does not use the Workspace image's Node for Program control code.

`packages/module-execution` supplies the same native source adapter to config,
declaration analysis and runtime. Installed JavaScript remains native; reached
TypeScript/JSX transforms in process with original source URLs. The runtime reads
source/export locators from the admitted Program index. Its entry and preload
belong to the Runtime artifact, not the customer Program.

Arbitrary Workspace commands retain their image tools and environment. No ambient
loader is injected into them. Public authoring APIs belong in `sdk/`; declaration
analysis belongs in `compiler/`; this layer owns execution and protocol handling.

Run the host protocol tests with `nix develop --command bun run --cwd
runtime/typescript test`. The separate `src/native-runtime.test.ts` suite needs a
Linux installation of `testdata/native-program` with the packed SDK, its runtime
dependencies, the local `mixed` package and a target-compatible SQLite addon.
Prepare and admit that tree with the bundle builder, then mount the admitted
Program at `/opt/helmr/program` and the matching Runtime at `/opt/helmr/runtime`,
both read-only. Bundle the test driver for Node with the repository generation
tools and run it with the Runtime's Node and `HELMR_NATIVE_RUNTIME_TEST=1`.
It checks Task/Actor protocol execution, assets, native addon loading, worker/fork
inheritance and process termination. Ordinary host tests skip this suite; passing
it in a container does not qualify Firecracker checkpoint/resume.

## External Runtime dependencies

`internal/version/runtime-dependencies.json` owns the exact Node release and its
four official archive hashes, plus the Runtime TypeScript npm version and archive
integrity. Nix and Go read it directly. `scripts/build-module-execution-entry.sh`
projects TypeScript into the private module-execution package manifest and source
constant; its existing `--check` mode rejects stale projections, Bun lock version
or integrity, and installed package version. After changing the authority, run the
generator, refresh the native lock/install with `bun install` if requested, then
run the generator again and its `--check` mode. Generated compiler/Runtime entry
checks invoke this same generator.

Root authoring TypeScript, website/example dependencies, Bun and Go keep their own
ownership. The loader remains authored in `packages/module-execution`; Runtime v0
metadata and compiler contracts still use digests computed from actual output
bytes, not hashes substituted from this external-input manifest.
