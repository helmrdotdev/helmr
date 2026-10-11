# Examples

The four basic projects below define Agents with a default Computer. Helmr builds
each project with the Manager and Managed Node selected by `package.json`. The
Computer image supplies tools launched by application code.

Deploy the project, then start an Agent with input. Each admission returns a
Session and Turn. Later Turns in that Session retain its Computer state. Validate
input in the handler and bind credentials by stable Environment Secret ID in the
Computer declaration. Relative file paths use the Computer working directory.

- `hello-world` — greeting, validated response and an answerable CI-report question.
- `cli-tooling` — invoke an installed CLI and return a report.
- `task-secrets` — bind a Secret ID and read the resulting environment variable.
- `github-pr-review` — read a GitHub pull request and return a summary.

Each README gives setup and admission commands. The standalone `issue-fixer`
project has separate provider dependencies and qualification instructions.
Runtime contract fixtures live under `tests/fixtures/`.
