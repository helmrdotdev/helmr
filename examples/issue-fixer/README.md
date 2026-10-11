# Issue fixer example

Editable Agent applications using retained native Codex and Claude sessions.
Read `tasks/issue-fixer/README.md` for repository preparation, stable Secret
bindings, current questions, managed MCP and lifecycle behavior.

The standalone project uses provider dependencies pinned in `package.json`.
From the Product root, `scripts/build-npm-packages.sh` builds its local SDK.
Inside this directory, run `bun install --frozen-lockfile --ignore-scripts`, then
`bun run typecheck` and `bun run test` through the repository's Nix environment.
`bun run test:native-probes` uses real native processes and loopback services;
it requires the pinned Node runtime. No SDK publication is required.

`tasks/agent-toolchain.ts` is an optional integration Agent that contacts configured
providers and a repository. Run it only when that exact integration is intended;
local tests do not invoke it. It is not an implicit release gate.
