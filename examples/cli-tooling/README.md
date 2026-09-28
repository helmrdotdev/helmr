# CLI Tooling

Install a command-line tool into a Computer image, then use it from a Task Run.
This example installs `ripgrep` with APT and runs `rg` from the Computer cwd
before writing a report.

```bash
helmr deploy PATH/TO/cli-tooling --project PROJECT --env ENVIRONMENT
```
