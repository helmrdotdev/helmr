# Examples

Runnable Helmr task projects live here. Each example is a small project that
shows one customer-facing workflow.

Helmr installs and builds each project with the exact Manager and Managed Node
selected by `package.json`. Task code runs with that Managed Node. Node inside a
Workspace image is separate tool authority used only by commands launched
through the Workspace environment.

Deploy the task source first, then start runs. Each run receives an empty writable
workspace. If a task needs external files or repository contents, pass identifiers
in payload and credentials through declared secrets.

Tasks start in the workspace directory. Use relative paths for workspace files;
absolute paths keep normal Linux container semantics.

## Included Examples

- `hello-world` — the smallest Task and Workspace shape with payload and file output.
- `dependency-cache` — dependency-layer image builds with a runtime workspace report.
- `cli-tooling` — install a CLI in the Workspace image and run it against the Workspace.
- `task-secrets` — read a Secret attached when the Workspace is created.
- `github-pr-review` — inspect a GitHub pull request and return a review summary.

Runtime contract task project fixtures live under `fixtures/`, not here.
