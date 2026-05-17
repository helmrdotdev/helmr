# CLI Tooling

Install a command-line tool into the sandbox image, then use it against the
GitHub checkout workspace. This example installs `ripgrep` with APT and runs
`rg` from the task's workspace cwd to discover task exports before writing a
workspace report.

```bash
helmr deploy PATH/TO/cli-tooling

helmr run cli-tooling \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/cli-tooling \
  --payload pattern='export const'
```
