# Examples

Runnable Helmr task projects live here. Each example is a small project that
shows one customer-facing workflow.

Most sandbox images here use Node as the TypeScript task runtime and install
task project dependencies with the package manager declared in `package.json`.
The examples install Bun explicitly as the package manager; task code itself
runs on the image-provided `node` executable.

Deploy the task source first, then start runs. Each run receives an empty writable
workspace. If a task needs external files or repository contents, pass identifiers
in payload and credentials through declared secrets.

Tasks start in the workspace directory. Use relative paths for workspace files;
absolute paths keep normal Linux container semantics.

## Included Examples

- `hello-world` — the smallest task shape: image, sandbox, payload, workspace output.
- `dependency-cache` — dependency-layer image builds with a runtime workspace report.
- `cli-tooling` — install a CLI in the sandbox image and run it against the workspace.
- `human-in-the-loop` — generic human waitpoints for dashboard-driven workflows.
- `task-secrets` — declared task secrets resolved from the selected project environment.
- `github-pr-review` — a GitHub PR workflow with a human-approved write action.

Runtime contract task project fixtures live under `fixtures/`, not here.
