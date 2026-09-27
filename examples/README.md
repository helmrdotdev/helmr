# Examples

Runnable Helmr task projects live here. Each example is a small project that
shows one customer-facing workflow.

Helmr installs and builds each project with the exact Manager and Managed Node
selected by `package.json`. Task code runs with that Managed Node. Node inside a
Computer image is separate tool authority used only by commands launched
through the Computer environment.

Deploy the task source first, then start runs. Each run receives an empty writable
computer. If a task needs external files or repository contents, pass identifiers
in payload and credentials through declared secrets.

Tasks start in the computer directory. Use relative paths for computer files;
absolute paths keep normal Linux container semantics.

## Included Examples

- `hello-world` — the smallest Task and Computer shape with payload and file output.
- `cli-tooling` — install a CLI in the Computer image and run it against the Computer.
- `task-secrets` — read a Secret attached when the Computer is created.
- `github-pr-review` — inspect a GitHub pull request and return a review summary.

Runtime contract task project fixtures live under `fixtures/`, not here.
