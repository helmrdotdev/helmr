# Module loader

`@helmr/module-loader` is the private Node module loader shared by declaration
analysis in `compiler/typescript` and managed Task/Actor execution in
`runtime/typescript`. It resolves source imports, transforms reached TypeScript
and JSX in memory, and keeps module source and configuration reads inside the
Program tree. Ordinary JavaScript loading stays with Node.

`src/index.ts` installs the Node hooks and exposes source exports and loader
identity. `src/authority.ts` owns contained paths and TypeScript configuration
reads. These checks constrain module loading; they are not a filesystem sandbox.
Task/Actor lifecycle and protocol handling belong to the runtime. Host
`helmr.config.ts` evaluation uses the separate compiler-side `jiti` evaluator.

Edit these sources, then run `scripts/build-module-loader-entry.sh` in the Nix
development shell. It generates `internal/moduleloader/loader.mjs` and the local
TypeScript payload; `--check` verifies the tracked projections and generated code.
The Nix compiler and Runtime package the same loader and TypeScript bytes, whose
digests form the `moduleLoader` identity in their artifact contracts.

After installing workspace dependencies and generating the loader, run
`bun run --cwd packages/module-loader test` and
`bun run --cwd packages/module-loader typecheck` in the Nix development shell.
