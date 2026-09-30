# Runtime

The TypeScript runtime executes SDK Task and Actor handlers and translates their
waits, streams, metadata and outcomes into the guest protocol. `guestd` launches
the verified platform-owned Node 24.21 runtime, fixed entry and module preload.
It does not use the Computer image's Node for Program control code.

The runtime imports generated JavaScript modules from the admitted Program's
`modulePath`/export locators. Native Node owns resolution and module formats. The
preload checks module provenance and rejects TypeScript execution; it does not
transform code or read the author's `tsconfig`. Compiler and runtime share this
provenance policy, not a TypeScript loader. The entry and preload belong to the
Runtime artifact, separately from the customer Program.

The Runtime carries its own C and C++ runtime so Node runs in any Computer
image, including musl ones: the loader, the glibc components and libstdc++ come
from a digest-pinned Debian stable image (`nix/packages/debian-images.json`).
Node depends on every glibc component directly, including those it does not
use itself, so an addon or Computer library that names one always receives the
Runtime's copy rather than a mismatched one from the image. Every other shared
library resolves as in an ordinary process: the object's RPATH, then the
Computer image's `ld.so.cache` and standard directories. The Computer image
owns those libraries; a missing or too-new one is the ordinary loader error.

Arbitrary Computer commands retain their image tools and environment. No ambient
loader is injected into them. Public authoring APIs belong in `sdk/`; declaration
analysis belongs in `compiler/`; this layer owns execution and protocol handling.

Run the host protocol tests with `nix develop --command bun run --cwd
runtime/typescript test`. The separate `src/native-runtime.test.ts` suite needs a
Linux installation of `testdata/native-program` with the packed SDK, its external runtime
dependencies and a target-compatible SQLite addon.
Prepare and admit that tree with the bundle builder, then mount the admitted
Program at `/opt/helmr/program` and the matching Runtime at `/opt/helmr/runtime`,
both read-only. Bundle the test driver for Node with the repository generation
tools and run it with the Runtime's Node and `HELMR_NATIVE_RUNTIME_TEST=1`.
It checks Task/Actor protocol execution, assets, native addon loading, process termination and the unchanged Computer working directory. Ordinary host tests skip this suite; passing
it in a container does not qualify Firecracker checkpoint/resume.

## External Runtime dependencies

`internal/version/runtime-dependencies.json` owns the exact Node release and its
four official archive hashes. Nix and Go read it directly. Runtime metadata binds
the actual preload bytes; the Runtime artifact contains no TypeScript compiler,
esbuild or npm. Those tools belong to the build or authoring environment.

Root authoring TypeScript, website/example dependencies, Bun and Go retain their
own version ownership. The Linux `nix build .#runtimeRelease` package generates
Runtime entries through the shared `platformEntries` package, then derives policy
and artifact digests from those exact bytes. For local generation, install locked
workspace dependencies and run `make platform-entries` in the pinned Nix shell;
`scripts/build-runtime-entry.sh` remains available for focused regeneration.
Generated platform entries are ignored by Git. Run the pinned Runtime artifact
verification after changing runtime code or external inputs.
