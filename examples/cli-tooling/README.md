# CLI Tooling

Install a command-line tool into the sandbox image, then use it from the run
workspace. This example installs `ripgrep` with APT and runs `rg` from the
task's workspace cwd before writing a workspace report.

```bash
helmr deploy PATH/TO/cli-tooling

helmr run cli-tooling \
  --payload pattern='export const'
```
