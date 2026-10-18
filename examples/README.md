# Examples

Examples show useful work you can give an Agent and how to continue that work.
Choose an outcome below, inspect the application code, and adapt it to your own
workspace. Each example owns its tools, model configuration and work procedure.
The harness choices illustrate implementations; they are not restrictions on
which harness can perform a task.

## Try a use case

| Example | Ask it to | Inspect the result | Follow up |
| --- | --- | --- | --- |
| [Code fix](issue-fixer/) | Fix a bug in a prepared repository and run its checks | A patch, validation results and an explanation | Refine the fix in the same Session |
| [Small website](opencode-assistant/) | Clarify a brief and build a small website with OpenCode | Local HTML, CSS and JavaScript files | Change the content or layout |
| [Sales analysis](pi-assistant/) | Compare two months of a bundled synthetic CSV with Pi | A reusable analysis script and Markdown report | Break the change down by product or region |

The code-fix project uses Codex or Claude. The website and analysis projects use
the same native SDK versions and lifecycle code as the corresponding starters,
with explicit application configuration. Read each README for its setup,
permission behavior and validation limits. Producing files does not publish a
site, post a GitHub review or open a pull request.

For a minimal starting project for a chosen harness, use
`helmr init --template <harness>` instead; see [starters](../starters/).

## Learn an individual feature

These small recipes explain a particular API rather than a complete model task:

- [Hello World](hello-world/) — Agent input, response, questions and retained files.
- [CLI tooling](cli-tooling/) — install and invoke a CLI in a Computer.
- [Computer Secrets](agent-secrets/) — bind a Secret ID without printing its value.
- [GitHub PR summary](github-pr-review/) — read PR metadata and summarize changed
  files; this does not perform a model review or post a review.

Runtime contract and fault-injection fixtures belong in `tests/fixtures/`.
