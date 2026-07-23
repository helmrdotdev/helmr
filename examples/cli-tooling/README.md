# CLI Tooling

Install a command-line tool into a Workspace image, then use it from a Task Run.
This example installs `ripgrep` with APT and runs `rg` from the Workspace cwd
before writing a report.

```bash
helmr deploy PATH/TO/cli-tooling
```
