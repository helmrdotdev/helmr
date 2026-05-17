# Examples

Runnable Helmr task projects live here. Each example is a small project that
shows one customer-facing workflow.

Most sandbox images here intentionally do not install Bun. Helmr injects its own
TypeScript runtime into the guest before running task code, so images only need
the operating-system tools and application dependencies the task itself uses.
`dependency-cache` is kept as the Bun dependency-cache example.

Deploy the task source first, then start runs against a GitHub workspace. The
workspace repository must have the Helmr GitHub App installed. If you are trying
an example from this repository, fork it first or copy the example into your own
repository, then replace `OWNER/REPO` and `PATH/TO/...` in the commands below.

Tasks start in the workspace directory. Use relative paths for workspace files;
absolute paths keep normal Linux container semantics.

## Included Examples

- `hello-world` — the smallest task shape: image, sandbox, payload, workspace output.
- `dependency-cache` — dependency-layer image builds with the GitHub checkout as the runtime workspace.
- `cli-tooling` — install a CLI in the sandbox image and run it against the workspace.
- `human-in-the-loop` — approval and message waitpoints for dashboard-driven workflows.
- `secret-vault` — declared task secrets bound from the Helmr remote secret vault.
- `github-pr-review` — a GitHub PR workflow with a human-approved write action.

Runtime contract task project fixtures live under `fixtures/`, not here.
